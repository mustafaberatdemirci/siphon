package site

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// PixeldrainName is the registry key.
const PixeldrainName = "pixeldrain"

// ExtraPixeldrainAPIKey is the [site.extra] api_key key in sites.toml.
//
// pixeldrain's own documentation: "Hotlinking is only allowed when either the
// uploader or the downloader has a premium subscription." A third-party
// downloader is a hotlink by definition. Once a file's download count exceeds
// three times its view count a captcha gate comes down, and it is a PER-FILE
// counter: changing IP (VPN) changes nothing.
//
// The designed way out is a paid account's API key. It is sent with HTTP
// Basic: empty username, key as password. It is added to both API calls and
// TRANSFER requests; the limit applies on the actual transfer.
//
// We DO NOT try to solve the captcha or inflate the view count: the first is
// out of scope, the second is tricking the site's access control.
const ExtraPixeldrainAPIKey = "api_key"

// NewPixeldrain is the factory given to the registry.
func NewPixeldrain(cfg SiteConfig) Resolver {
	cfg = cfg.WithDefaults()
	return &pixeldrain{
		cfg:    cfg,
		apiKey: strings.TrimSpace(cfg.ExtraOr(ExtraPixeldrainAPIKey, "")),
	}
}

type pixeldrain struct {
	cfg    SiteConfig
	apiKey string // anonymous if empty
}

// authHeader produces the HTTP Basic header value if there is an API key.
// The key is NEVER written to logs, recordings or error evidence; only the header.
func (p *pixeldrain) authHeader() string {
	if p.apiKey == "" {
		return ""
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+p.apiKey))
}

// APIError is pixeldrain's error envelope. The Value field is what decisions
// are based on; message is for humans and may change.
//
// Known value codes (all 403 unless noted otherwise):
// file_rate_limited_captcha_required, virus_detected_captcha_required,
// hotlink_detected, ip_download_limited_captcha_required,
// max_concurrent_downloads, transfer_limit_exceeded, download_limit_exceeded,
// unavailable_for_legal_reasons (451), not_found (404), recpatcha_failed (424).
type APIError struct {
	Status  int
	Value   string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("pixeldrain %d %s: %s", e.Status, e.Value, e.Message)
}

// Retryable reports whether the error is worth waiting and retrying.
// The backoff layer from step 6 reads it. Codes asking for a captcha are NOT
// retryable: waiting doesn't solve them, the user has to be told.
func (e *APIError) Retryable() bool {
	switch e.Value {
	case "transfer_limit_exceeded", "download_limit_exceeded", "max_concurrent_downloads":
		return true
	default:
		return false
	}
}

// CaptchaRequired covers the cases where the tool must stop and tell the user.
// Scope boundary: we don't try to solve captchas.
func (e *APIError) CaptchaRequired() bool {
	return strings.HasSuffix(e.Value, "_captcha_required") || e.Value == "recpatcha_failed"
}

type refKind int

const (
	refAlbum refKind = iota
	refFile
)

type ref struct {
	kind refKind
	id   string
	host string // the host requests go to; the input URL's own host
}

// Match reports whether the URL belongs to this resolver.
// LegacyDomains are accepted too: a link from a dead domain must be
// RECOGNIZED, but no request should go to that domain.
func (p *pixeldrain) Match(u string) bool {
	_, err := p.parse(u)
	return err == nil
}

func (p *pixeldrain) parse(raw string) (ref, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ref{}, errors.New("empty URL")
	}
	if !strings.Contains(raw, "//") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ref{}, fmt.Errorf("could not parse URL: %w", err)
	}
	host := normalizeHost(u.Host)
	known := MatchHost(host, p.cfg.Domains) || MatchHost(host, p.cfg.LegacyDomains)
	if !known {
		return ref{}, fmt.Errorf("unknown host: %s", host)
	}

	seg := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(seg) == 0 || seg[0] == "" {
		return ref{}, errors.New("empty path")
	}

	switch seg[0] {
	case "l":
		if len(seg) < 2 || seg[1] == "" {
			return ref{}, errors.New("no album id")
		}
		return ref{kind: refAlbum, id: seg[1], host: host}, nil
	case "u":
		if len(seg) < 2 || seg[1] == "" {
			return ref{}, errors.New("no file id")
		}
		return ref{kind: refFile, id: seg[1], host: host}, nil
	case "api":
		// /api/file/{id} and /api/file/{id}/info
		if len(seg) >= 3 && seg[1] == "file" && seg[2] != "" {
			return ref{kind: refFile, id: seg[2], host: host}, nil
		}
		if len(seg) >= 3 && seg[1] == "list" && seg[2] != "" {
			return ref{kind: refAlbum, id: seg[2], host: host}, nil
		}
		return ref{}, errors.New("unsupported api path")
	default:
		// Short link of the form pixeldra.in/{id}. Only valid as a single segment.
		if len(seg) == 1 {
			return ref{kind: refFile, id: seg[0], host: host}, nil
		}
		return ref{}, fmt.Errorf("unsupported path: /%s", strings.Join(seg, "/"))
	}
}

// fetchHost picks the host requests go to. If the input URL comes from a dead
// domain (LegacyDomains) no request is sent to that host; the first entry of
// the active list is used.
func (p *pixeldrain) fetchHost(r ref) (string, error) {
	if MatchHost(r.host, p.cfg.Domains) {
		return r.host, nil
	}
	if len(p.cfg.Domains) == 0 {
		return "", errors.New("active domain list is empty")
	}
	first := p.cfg.Domains[0]
	if strings.ContainsRune(first, '*') {
		return "", fmt.Errorf("the first entry of the active domain list is a wildcard: %q", first)
	}
	return normalizeHost(first), nil
}

func (p *pixeldrain) client() *http.Client {
	if p.cfg.HTTPClient != nil {
		return p.cfg.HTTPClient
	}
	return http.DefaultClient
}

// get makes an API call. pageURL is the HUMAN page used as the Referer when
// the policy is "item_page"; not the API URL being requested. Mixing the two
// makes the Referer meaningless (and breaks bunkr outright).
func (p *pixeldrain) get(ctx context.Context, rawURL, pageURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Errorf(LayerFetch, rawURL, "could not build request: %v", err)
	}
	if p.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", p.cfg.UserAgent)
	}
	if a := p.authHeader(); a != "" {
		req.Header.Set("Authorization", a)
	}
	// On pixeldrain RefererPolicy must be "none": a wrong Referer triggers
	// exactly hotlink_detected. If the policy is explicitly origin/item_page
	// it is applied.
	switch p.cfg.RefererPolicy {
	case RefererOrigin:
		req.Header.Set("Referer", "https://"+req.URL.Host+"/")
	case RefererItemPage:
		if pageURL != "" {
			req.Header.Set("Referer", pageURL)
		}
	}

	resp, err := p.client().Do(req)
	if err != nil {
		return classifyTransportError(rawURL, err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if readErr != nil {
		return Errorf(LayerFetch, rawURL, "could not read body: %v", readErr)
	}

	if resp.StatusCode != http.StatusOK {
		apiErr := &APIError{Status: resp.StatusCode}
		var env struct {
			Value   string `json:"value"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &env) == nil && env.Value != "" {
			apiErr.Value, apiErr.Message = env.Value, env.Message
		} else {
			apiErr.Value = "http_" + resp.Status
			apiErr.Message = strings.TrimSpace(string(body[:min(len(body), 200)]))
		}
		layer := LayerFetch
		if resp.StatusCode == http.StatusForbidden && looksLikeChallenge(resp, body) {
			layer = LayerChallenge
		}
		return &LayerError{Layer: layer, Err: apiErr, Evidence: rawURL}
	}

	if err := json.Unmarshal(body, out); err != nil {
		return Errorf(LayerParse, rawURL, "could not decode JSON: %v", err)
	}
	return nil
}

// getRaw follows the same path as get but returns the raw body instead of a
// decoded structure. Needed for doctor's --record: what gets recorded is the
// bytes the server actually sent, not the decoded structure.
func (p *pixeldrain) getRaw(ctx context.Context, rawURL string) ([]byte, error) {
	var raw json.RawMessage
	if err := p.get(ctx, rawURL, "", &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// classifyTransportError ties a transport-level error to the right Layer.
// The DNS vs TLS distinction is critical: both look like "the site won't
// open" but their fixes differ.
func classifyTransportError(rawURL string, err error) error {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return &LayerError{Layer: LayerDNS, Err: err, Evidence: rawURL}
	}
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	if errors.As(err, &certErr) || errors.As(err, &unknownAuth) || errors.As(err, &hostErr) {
		return &LayerError{
			Layer: LayerTLS, Err: err,
			Evidence: rawURL + " (certificate could not be verified; the ISP may be intercepting)",
		}
	}
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return &LayerError{Layer: LayerTLS, Err: err, Evidence: rawURL}
	}
	return &LayerError{Layer: LayerFetch, Err: err, Evidence: rawURL}
}

// ClassifyStatus lets the downloader interpret 403/410 responses correctly.
// It satisfies site.StatusClassifier.
//
// Without it every 403 counts as "signed URL expired" and the tool would
// re-resolve the URL and retry while rate limited. pixeldrain, however, also
// reports transfer_limit_exceeded, hotlink_detected and *_captcha_required
// with 403.
func (p *pixeldrain) ClassifyStatus(resp *http.Response, body []byte) error {
	var env struct {
		Value   string `json:"value"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &env) != nil || env.Value == "" {
		// Not a recognizable API envelope: it may be the CDN rejecting a
		// signed URL. Returning nil means "apply the default".
		return nil
	}
	evidence := ""
	if resp.Request != nil && resp.Request.URL != nil {
		evidence = resp.Request.URL.String()
	}
	layer := LayerFetch
	if resp.StatusCode == http.StatusForbidden && looksLikeChallenge(resp, body) {
		layer = LayerChallenge
	}
	return &LayerError{
		Layer:    layer,
		Err:      &APIError{Status: resp.StatusCode, Value: env.Value, Message: env.Message},
		Evidence: evidence,
	}
}

// looksLikeChallenge reports whether a 403 is a Cloudflare challenge.
func looksLikeChallenge(resp *http.Response, body []byte) bool {
	if resp.Header.Get("CF-Mitigated") != "" {
		return true
	}
	low := strings.ToLower(string(body[:min(len(body), 4096)]))
	for _, m := range []string{"just a moment", "cf_chl", "challenge-platform", "enable javascript and cookies"} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

type fileInfo struct {
	Success    bool   `json:"success"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	MimeType   string `json:"mime_type"`
	HashSHA256 string `json:"hash_sha256"`
}

type listInfo struct {
	Success   bool       `json:"success"`
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	FileCount int        `json:"file_count"`
	Files     []fileInfo `json:"files"`
}

func (p *pixeldrain) apiBase(host string) string { return "https://" + host + "/api" }

// downloadURL is the file's real download URL. On pixeldrain this URL is NOT
// time-limited or signed, so ResolveOne is only there for completeness.
func (p *pixeldrain) downloadURL(host, id string) string {
	return p.apiBase(host) + "/file/" + url.PathEscape(id) + "?download"
}

func (p *pixeldrain) itemPage(host, id string) string {
	return "https://" + host + "/u/" + url.PathEscape(id)
}

func (p *pixeldrain) albumPage(host, id string) string {
	return "https://" + host + "/l/" + url.PathEscape(id)
}

// itemHeaders derives the headers to add to the download request from the policy.
//
// Without this, referer_policy would only affect API calls and never reach
// the actual transfer request. On pixeldrain the policy is "none" so the
// result is empty; but bunkr makes the item page Referer MANDATORY on the
// transfer, so the mechanism has to be in the right place already.
func (p *pixeldrain) itemHeaders(itemPage string) map[string]string {
	h := map[string]string{}
	switch p.cfg.RefererPolicy {
	case RefererItemPage:
		if itemPage != "" {
			h["Referer"] = itemPage
		}
	case RefererOrigin:
		if u, err := url.Parse(itemPage); err == nil && u.Host != "" {
			h["Referer"] = u.Scheme + "://" + u.Host + "/"
		}
	}
	// The paid account's key goes to the transfer request too: the hotlink
	// and captcha limits apply exactly on this request, not on the API call.
	if a := p.authHeader(); a != "" {
		h["Authorization"] = a
	}
	if len(h) == 0 {
		return nil
	}
	return h
}

// Resolve sends a SINGLE request for an album and produces Items from the
// embedded files[] array.
//
// /info is not called for each file: a 200-file album would take 201
// requests and trigger Premise 3's rate limit with our own hands.
//
// It costs nothing: pixeldrain's official API docs list the files[] schema
// incompletely, but the real response carries a filled hash_sha256 (verified
// 34/34 on a 34-file album). So sha256 verification works for album members
// too and a bulk /info call is never needed.
func (p *pixeldrain) Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error) {
	r, err := p.parse(u)
	if err != nil {
		return nil, Errorf(LayerParse, u, "%v", err)
	}
	host, err := p.fetchHost(r)
	if err != nil {
		return nil, Errorf(LayerParse, u, "%v", err)
	}

	if r.kind == refFile {
		item, err := p.resolveFile(ctx, host, r.id)
		if err != nil {
			return nil, err
		}
		if err := yield(item); err != nil {
			return nil, err
		}
		return nil, nil
	}

	var list listInfo
	if err := p.get(ctx, p.apiBase(host)+"/list/"+url.PathEscape(r.id),
		p.albumPage(host, r.id), &list); err != nil {
		return nil, err
	}
	if !list.Success {
		return nil, Errorf(LayerParse, u, "list returned success=false")
	}

	dir := sanitizeDirLabel(list.Title, list.ID)
	var itemErrs []ItemError

	// If file_count and the length of files[] diverge, the list arrived
	// incomplete. Silently downloading fewer files and exiting 0 would directly
	// violate the "no silent failure" principle; we report it as an error
	// without failing the album.
	if list.FileCount > 0 && list.FileCount != len(list.Files) {
		itemErrs = append(itemErrs, ItemError{
			URL: u,
			Err: Errorf(LayerParse, fmt.Sprintf("file_count=%d, files[]=%d",
				list.FileCount, len(list.Files)), "list arrived incomplete"),
		})
	}
	for i, f := range list.Files {
		if f.ID == "" {
			itemErrs = append(itemErrs, ItemError{
				URL: u,
				Err: Errorf(LayerParse, fmt.Sprintf("files[%d]", i), "empty id"),
			})
			continue
		}
		sourcePage := p.itemPage(host, f.ID)
		item := Item{
			URL:        p.downloadURL(host, f.ID),
			SourcePage: sourcePage,
			Headers:    p.itemHeaders(sourcePage),
			Dir:        dir,
			Filename:   f.Name,
			SHA256:     f.HashSHA256, // filled in the list response; the downloader verifies it
			Size:       f.Size,
			Index:      i,
		}
		if item.Size == 0 {
			item.Size = -1
		}
		if err := yield(item); err != nil {
			return itemErrs, err
		}
	}
	return itemErrs, nil
}

func (p *pixeldrain) resolveFile(ctx context.Context, host, id string) (Item, error) {
	var info fileInfo
	if err := p.get(ctx, p.apiBase(host)+"/file/"+url.PathEscape(id)+"/info",
		p.itemPage(host, id), &info); err != nil {
		return Item{}, err
	}
	if !info.Success {
		return Item{}, Errorf(LayerParse, id, "info returned success=false")
	}
	size := info.Size
	if size == 0 {
		size = -1
	}
	sourcePage := p.itemPage(host, info.ID)
	return Item{
		URL:        p.downloadURL(host, info.ID),
		SourcePage: sourcePage,
		Headers:    p.itemHeaders(sourcePage),
		Dir:        "",
		Filename:   info.Name,
		SHA256:     info.HashSHA256,
		Size:       size,
		Index:      0,
	}, nil
}

// ResolveOne re-resolves a single Item from its item page.
// On pixeldrain the download URL isn't signed, so in practice it isn't
// needed; it works correctly here because it is part of the contract and
// vital on bunkr.
func (p *pixeldrain) ResolveOne(ctx context.Context, sourcePage string) (Item, error) {
	r, err := p.parse(sourcePage)
	if err != nil {
		return Item{}, Errorf(LayerParse, sourcePage, "%v", err)
	}
	if r.kind != refFile {
		return Item{}, Errorf(LayerParse, sourcePage, "expected an item page, got an album")
	}
	host, err := p.fetchHost(r)
	if err != nil {
		return Item{}, Errorf(LayerParse, sourcePage, "%v", err)
	}
	return p.resolveFile(ctx, host, r.id)
}

// Diagnose tries the canary list in order and stops at the first one that works.
// It is not tied to a single canary: measurements showed a domain can be
// blocked by the ISP while the site itself still works.
func (p *pixeldrain) Diagnose(ctx context.Context) ([]LayerResult, error) {
	if len(p.cfg.CanaryURLs) == 0 {
		return nil, errors.New("canary URL list is empty")
	}

	var last []LayerResult
	for _, canary := range p.cfg.CanaryURLs {
		res := p.diagnoseOne(ctx, canary)
		last = res
		if !hasFail(res) {
			return res, nil
		}
	}
	return last, nil
}

func (p *pixeldrain) diagnoseOne(ctx context.Context, canary string) []LayerResult {
	out := make([]LayerResult, 0, 5)

	u, err := url.Parse(canary)
	if err != nil {
		return append(out, LayerResult{
			Layer: LayerDNS, Status: StatusFail,
			Detail: "could not parse the canary URL", Evidence: canary,
		})
	}
	host := normalizeHost(u.Host)

	addrs, dnsErr := net.DefaultResolver.LookupIPAddr(ctx, host)
	if dnsErr != nil {
		return append(out, LayerResult{
			Layer: LayerDNS, Status: StatusFail,
			Detail: "did not resolve", Evidence: host + ": " + dnsErr.Error(),
		})
	}
	ips := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP.String())
	}
	out = append(out, LayerResult{
		Layer: LayerDNS, Status: StatusOK,
		Detail: fmt.Sprintf("%d addresses", len(ips)), Evidence: strings.Join(ips, ", "),
	})

	var probe struct {
		Success bool `json:"success"`
	}
	raw, rawErr := p.getRaw(ctx, canary)
	p.cfg.Recordln("canary.json", raw)
	err = rawErr
	if err == nil {
		if jerr := json.Unmarshal(raw, &probe); jerr != nil {
			err = Errorf(LayerParse, canary, "could not decode JSON: %v", jerr)
		}
	}

	layer, _ := LayerOf(err)
	switch {
	case err == nil:
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "handshake OK"},
			LayerResult{Layer: LayerChallenge, Status: StatusOK, Detail: "no challenge"},
			LayerResult{Layer: LayerFetch, Status: StatusOK,
				Detail: fmt.Sprintf("200, %d bytes", len(raw))},
			LayerResult{Layer: LayerParse, Status: StatusOK, Detail: "JSON decoded"},
			// pixeldrain has no separate item page: the list response carries
			// file ids directly, the chain is a single step.
			LayerResult{Layer: LayerItemPage, Status: StatusOK,
				Detail: "pixeldrain has no separate item page, the chain is a single step"},
			// No separate CDN either: files come from the site's own domain,
			// so there is no rotating host set to watch. Not a gap, it is the
			// site's architecture.
			LayerResult{Layer: LayerCDN, Status: StatusOK,
				Detail: "files are served from the site domain, no separate CDN"},
		)
	case layer == LayerTLS:
		out = append(out, LayerResult{
			Layer: LayerTLS, Status: StatusFail,
			Detail: "certificate/handshake failed", Evidence: err.Error(),
		})
	case layer == LayerChallenge:
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "handshake OK"},
			LayerResult{Layer: LayerChallenge, Status: StatusFail,
				Detail: "Cloudflare challenge", Evidence: err.Error()},
		)
	case layer == LayerParse:
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "handshake OK"},
			LayerResult{Layer: LayerChallenge, Status: StatusOK, Detail: "no challenge"},
			LayerResult{Layer: LayerFetch, Status: StatusOK, Detail: "200"},
			LayerResult{Layer: LayerParse, Status: StatusFail,
				Detail: "response is not JSON", Evidence: err.Error()},
		)
	default:
		out = append(out, LayerResult{
			Layer: layer, Status: StatusFail,
			Detail: "request failed", Evidence: err.Error(),
		})
	}
	return out
}

func hasFail(rs []LayerResult) bool {
	for _, r := range rs {
		if r.Status == StatusFail {
			return true
		}
	}
	return false
}

// sanitizeDirLabel produces a raw label for the album folder.
// Full Windows sanitizing happens in step 5, in internal/dl/names.go; here
// only path separators are neutralized so Dir stays a single component.
func sanitizeDirLabel(title, id string) string {
	t := strings.TrimSpace(title)
	t = strings.NewReplacer("/", "-", "\\", "-", "\x00", "").Replace(t)
	t = strings.TrimSpace(t)
	if t == "" {
		return id
	}
	return t
}
