package site

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BunkrName is the registry key.
const BunkrName = "bunkr"

// Extra keys and their defaults.
//
// These are bunkr's most frequently changing parts. In 2025-02 the download
// URL was in the HTML's <source src>; in 2025-03 it became
// get.bunkrr.su/api/vs; then apidl.bunkr.ru/api/_001_v2; as of 2026-09-10
// dl.bunkr.cr/api/_001_v2 PLUS a mandatory signing service. All of them live
// in config so the next change doesn't require a code change.
const (
	ExtraAPIEndpoint  = "api_endpoint"
	ExtraFallbackAPI  = "fallback_api_endpoint"
	ExtraDLOrigin     = "dl_origin"
	ExtraXORPrefix    = "xor_key_prefix"
	ExtraSignEndpoint = "sign_endpoint"
	ExtraLegacyPrefix = "legacy_path_prefix"

	defaultBunkrAPIEndpoint = "https://dl.bunkr.cr/api/_001_v2"
	defaultBunkrDLOrigin    = "https://dl.bunkr.cr"
	defaultBunkrXORPrefix   = "SECRET_KEY_"

	// Fallback endpoint: used when the primary endpoint is UNREACHABLE. On
	// this network dl.bunkr.cr is hijacked by the ISP at the DNS level, while
	// apidl.bunkr.ru is open (2026-09-10 measurement).
	defaultBunkrFallbackAPI = "https://apidl.bunkr.ru/api/_001_v2"

	// Storage prefix added to the path given by the old endpoint. Details in applyLegacyPrefix.
	defaultBunkrLegacyPrefix = "/storage/media"

	// Signing service. The CDN rejects unsigned requests with 403 without
	// looking at the file: the response for an existing and a nonexistent
	// file is byte-for-byte identical. 2026-09-10 measurement.
	defaultBunkrSignEndpoint = "https://glb-apisign.cdn.cr/sign"
)

// NewBunkr is the factory given to the registry.
func NewBunkr(cfg SiteConfig) Resolver {
	cfg = cfg.WithDefaults()
	b := &bunkr{
		cfg:          cfg,
		apiEndpoint:  cfg.ExtraOr(ExtraAPIEndpoint, defaultBunkrAPIEndpoint),
		dlOrigin:     strings.TrimRight(cfg.ExtraOr(ExtraDLOrigin, defaultBunkrDLOrigin), "/"),
		xorPrefix:    cfg.ExtraOr(ExtraXORPrefix, defaultBunkrXORPrefix),
		signEndpoint: cfg.ExtraOr(ExtraSignEndpoint, defaultBunkrSignEndpoint),
		fallbackAPI:  cfg.ExtraOr(ExtraFallbackAPI, defaultBunkrFallbackAPI),
		legacyPrefix: strings.TrimRight(cfg.ExtraOr(ExtraLegacyPrefix, defaultBunkrLegacyPrefix), "/"),
		burned:       map[string]burnInfo{},
		now:          time.Now,
	}
	// An unrecognized extra key is WARNED about. ExtraOr silently falls back
	// to the in-code default for a missing key, so a typo like "sign_endpont"
	// would be ignored without any symptom: you'd believe the fix you wrote
	// into the config was applied.
	known := map[string]bool{
		ExtraAPIEndpoint: true, ExtraFallbackAPI: true, ExtraDLOrigin: true,
		ExtraXORPrefix: true, ExtraSignEndpoint: true, ExtraLegacyPrefix: true,
	}
	for k := range cfg.Extra {
		if !known[k] {
			cfg.Logln("bunkr: unrecognized extra key %q in config — ignored (possibly a typo)", k)
		}
	}

	// The rotation pool is built from CONCRETE domains; wildcard entries only
	// serve recognition and can't be picked at random.
	for _, d := range cfg.Domains {
		if !strings.ContainsRune(d, '*') {
			b.all = append(b.all, normalizeHost(d))
		}
	}
	return b
}

type bunkr struct {
	cfg          SiteConfig
	apiEndpoint  string
	dlOrigin     string
	xorPrefix    string
	signEndpoint string
	fallbackAPI  string
	legacyPrefix string

	// primaryDeadAt is when the primary API endpoint was found unreachable;
	// zero means the endpoint is healthy. STICKY but TIME-LIMITED: sticky,
	// because otherwise a separate timeout would be waited for every file of
	// an album; time-limited, because the cause may be temporary (VPN,
	// network) and the window stays open for days. After primaryRetryAfter
	// the primary is tried again.
	primaryDeadAt time.Time

	// Rotation state. NOT global (in gallery-dl it is a package-level set);
	// tying it to the resolver instance makes parallel tests possible.
	mu     sync.Mutex
	all    []string            // concrete domains, in config ORDER
	burned map[string]burnInfo // domain -> reason and duration of elimination

	// now is the clock for elimination durations; tests move it forward.
	now func() time.Time
}

// burnInfo records why and until when a domain is eliminated.
type burnInfo struct {
	reason string
	until  time.Time
}

// Elimination durations. Elimination used to be permanent for the process
// lifetime: with the window open, even the few seconds of DNS outage that
// switching VPN causes eliminated EVERY domain and bunkr stayed dead until
// the app was restarted.
//
// There are two classes because they mean different things: failing to
// connect (DNS, timeout, refused) can be a temporary state of the local
// network and should be retried soon; a Cloudflare challenge or a certificate
// error is a real BLOCK by the site or the ISP and shouldn't be retried for
// nothing every two minutes.
const (
	burnTTLNetwork    = 2 * time.Minute
	burnTTLBlock      = 30 * time.Minute
	primaryRetryAfter = 10 * time.Minute
)

// ---------- URL recognition ----------

type bunkrKind int

const (
	bunkrAlbum bunkrKind = iota
	bunkrMedia
)

type bunkrRef struct {
	kind bunkrKind
	id   string // album id or media slug
	seg  string // path prefix for media: f, v, i, d
	host string
}

func (b *bunkr) Match(u string) bool {
	_, err := b.parse(u)
	return err == nil
}

func (b *bunkr) parse(raw string) (bunkrRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return bunkrRef{}, errors.New("empty URL")
	}
	if !strings.Contains(raw, "//") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return bunkrRef{}, fmt.Errorf("could not parse URL: %w", err)
	}
	host := normalizeHost(u.Host)
	host = strings.TrimPrefix(host, "app.")

	known := MatchHost(host, b.cfg.Domains) ||
		MatchHost(host, b.cfg.LegacyDomains) ||
		MatchHost(host, b.cfg.MatchPatterns)
	if !known {
		return bunkrRef{}, fmt.Errorf("unknown host: %s", host)
	}

	seg := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(seg) < 2 || seg[1] == "" {
		return bunkrRef{}, errors.New("incomplete path")
	}
	switch seg[0] {
	case "a":
		return bunkrRef{kind: bunkrAlbum, id: seg[1], host: host}, nil
	case "f", "v", "i", "d":
		return bunkrRef{kind: bunkrMedia, id: seg[1], seg: seg[0], host: host}, nil
	default:
		return bunkrRef{}, fmt.Errorf("unsupported path: /%s", strings.Join(seg, "/"))
	}
}

// ---------- Domain rotation ----------

// ErrAllDomainsBurned is returned when every domain has been eliminated.
var ErrAllDomainsBurned = errors.New("every bunkr domain has been eliminated")

// roots returns the domains to try in rotation, in config ORDER. Expired
// eliminations are cleared here; the domain returns to its old place in the
// list (order matters: the working domain first).
func (b *bunkr) roots() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	out := make([]string, 0, len(b.all))
	for _, d := range b.all {
		if info, burned := b.burned[d]; burned {
			if now.Before(info.until) {
				continue
			}
			delete(b.burned, d)
			b.cfg.Logln("bunkr: elimination of %s expired, back in rotation", d)
		}
		out = append(out, d)
	}
	return out
}

// burn takes a domain out of the rotation pool for ttl and records the
// reason. Why record it: saying "bunkr.cr ISP block, bunkr.black Cloudflare
// challenge" in doctor's output is a completely different thing for diagnosis
// than "3 domains didn't work".
func (b *bunkr) burn(domain, reason string, ttl time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if info, done := b.burned[domain]; done && now.Before(info.until) {
		return
	}
	b.burned[domain] = burnInfo{reason: reason, until: now.Add(ttl)}
	left := 0
	for _, d := range b.all {
		if info, burned := b.burned[d]; !burned || !now.Before(info.until) {
			left++
		}
	}
	b.cfg.Logln("bunkr: %s eliminated for %s (%s); domains left: %d", domain, FormatWait(ttl), reason, left)
}

// Burned returns the currently eliminated domains and their reasons (for doctor and logs).
func (b *bunkr) Burned() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	out := make(map[string]string, len(b.burned))
	for k, v := range b.burned {
		if now.Before(v.until) {
			out[k] = v.reason
		}
	}
	return out
}

// burnTTL derives how long an elimination lasts from the error class: a
// block (challenge, certificate, corrupt TLS record) is long, failing to
// connect is short.
func burnTTL(err error) time.Duration {
	var ch *challengeError
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var recordErr tls.RecordHeaderError
	if errors.As(err, &ch) || errors.As(err, &certErr) || errors.As(err, &unknownAuth) ||
		errors.As(err, &hostErr) || errors.As(err, &recordErr) {
		return burnTTLBlock
	}
	return burnTTLNetwork
}

// burnReason reports whether an error requires eliminating the domain.
//
// gallery-dl eliminates ONLY on 403. A measurement (2026-09-09) showed that
// this is incomplete: from an ISP network in Turkey most bunkr domains are
// SNI-blocked and the block arrives NOT as a 403 but as a certificate
// verification failure or a connection timeout. Handling all three together
// makes rotation work against both Cloudflare and ISP blocks.
func burnReason(err error) (string, bool) {
	var ch *challengeError
	if errors.As(err, &ch) {
		return "Cloudflare challenge (403)", true
	}

	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	if errors.As(err, &certErr) || errors.As(err, &unknownAuth) || errors.As(err, &hostErr) {
		return "certificate could not be verified (the ISP may be intercepting)", true
	}
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return "corrupt TLS record (a middlebox in the way)", true
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "DNS did not resolve", true
	}

	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return "connection timeout (may be an SNI filter)", true
	}

	// Any connection-level error (refused, unreachable, reset) means this
	// host is unusable. Since the HTTP layer was never reached nothing can be
	// said about the content; the only right reaction is trying another domain.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "could not connect: " + collapseSpace(opErr.Err.Error()), true
	}
	return "", false
}

// collapseSpace squeezes multi-line system error messages onto one line.
// Windows network errors contain embedded line breaks and wrap the log
// output; keeping a diagnostic line on one line matters for readability.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// challengeError separates a 403 from other HTTP errors.
type challengeError struct {
	url  string
	body []byte
}

func (e *challengeError) Error() string { return "403: Cloudflare challenge: " + e.url }

// ---------- HTTP ----------

func (b *bunkr) client() *http.Client {
	if b.cfg.HTTPClient != nil {
		return b.cfg.HTTPClient
	}
	return http.DefaultClient
}

// get fetches a single URL. It handles redirects BY HAND.
//
// Automatic redirect following is off because a Cloudflare challenge can
// also arrive as a redirect and automatic following swallows that signal.
// gallery-dl uses allow_redirects=False for the same reason.
func (b *bunkr) get(ctx context.Context, rawURL, referer string) ([]byte, error) {
	const maxHops = 8
	cur := rawURL
	for hop := 0; hop < maxHops; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur, nil)
		if err != nil {
			return nil, Errorf(LayerFetch, cur, "could not build request: %v", err)
		}
		b.setHeaders(req, referer)

		resp, err := b.noRedirect().Do(req)
		if err != nil {
			return nil, unwrapURLError(err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if readErr != nil {
				return nil, Errorf(LayerFetch, cur, "could not read body: %v", readErr)
			}
			return body, nil

		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			loc := resp.Header.Get("Location")
			if loc == "" {
				return nil, Errorf(LayerFetch, cur, "%d but no Location", resp.StatusCode)
			}
			next, err := resolveLocation(cur, loc)
			if err != nil {
				return nil, Errorf(LayerFetch, cur, "could not resolve Location: %v", err)
			}
			cur = next
			continue

		case resp.StatusCode == http.StatusForbidden:
			return nil, &challengeError{url: cur, body: body}

		default:
			return nil, Errorf(LayerFetch, cur, "HTTP %s", resp.Status)
		}
	}
	return nil, Errorf(LayerFetch, rawURL, "gave up after %d redirects", maxHops)
}

// noRedirect returns a copy with automatic redirect following turned off.
func (b *bunkr) noRedirect() *http.Client {
	c := *b.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &c
}

func (b *bunkr) setHeaders(req *http.Request, referer string) {
	if b.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", b.cfg.UserAgent)
	}
	switch b.cfg.RefererPolicy {
	case RefererItemPage:
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
	case RefererOrigin:
		req.Header.Set("Referer", "https://"+req.URL.Host+"/")
	}
}

func resolveLocation(base, loc string) (string, error) {
	bu, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	lu, err := url.Parse(loc)
	if err != nil {
		return "", err
	}
	return bu.ResolveReference(lu).String(), nil
}

// unwrapURLError opens the *url.Error wrapper so TLS/DNS types can be seen
// with errors.As.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue
	}
	return err
}

// fetchWithRotation tries path until it finds a working domain.
//
// The first return value is the body, the second the root the request went
// to ("https://bunkr.ws"). The root is returned because subsequent requests
// (item page) must stay on the same domain; retrying the rotation from the
// start on every request would add needless latency.
func (b *bunkr) fetchWithRotation(ctx context.Context, path, referer string) ([]byte, string, error) {
	roots := b.roots()
	if len(roots) == 0 {
		return nil, "", b.allBurnedError()
	}
	var last error
	for i, d := range roots {
		root := "https://" + d
		if i > 0 {
			b.cfg.Logln("bunkr: trying %s (%d/%d)", d, i+1, len(roots))
		}
		body, err := b.get(ctx, root+path, referer)
		if err == nil {
			// Which domain served is ALWAYS logged: "which domain was used" is
			// the first question asked about a rotating tool.
			b.cfg.Logln("bunkr: %s served%s", d, ordinal(i))
			return body, root, nil
		}
		b.cfg.Logln("bunkr: %s failed: %s", d, collapseSpace(err.Error()))
		last = err
		if reason, ok := burnReason(err); ok {
			b.burn(d, reason, burnTTL(err))
			continue
		}
		// Permanent error (like 404): the domain is fine, the content is not
		// there. Rotating is pointless.
		return nil, "", err
	}
	if len(b.roots()) == 0 {
		return nil, "", b.allBurnedError()
	}
	return nil, "", last
}

func (b *bunkr) allBurnedError() error {
	// Burned() takes the lock itself; the map isn't touched here without it.
	burned := b.Burned()
	details := make([]string, 0, len(burned))
	for d, why := range burned {
		details = append(details, d+": "+why)
	}
	sort.Strings(details)
	return &LayerError{
		Layer:    LayerChallenge,
		Err:      ErrAllDomainsBurned,
		Evidence: strings.Join(details, "; "),
	}
}

// ---------- Album parsing ----------

type bunkrFile struct {
	ID       string
	Name     string
	Slug     string
	Size     int64
	Ext      string
	MimeType string
}

// parseAlbumFiles parses the window.albumFiles array on the album page.
//
// The source is NOT JSON but embedded JavaScript: unquoted keys and
// line-ending commas. gallery-dl reads it by matching exact separators per
// field (" id: " and "size:  ", double space included). A more tolerant path
// was chosen here on purpose: every line is parsed as "key: value,". Exact
// whitespace matching is precisely the first thing to break, and this file
// exists to make that breakage cheap.
func parseAlbumFiles(page string) ([]bunkrFile, error) {
	const marker = "window.albumFiles"
	i := strings.Index(page, marker)
	if i < 0 {
		return nil, errors.New("window.albumFiles not found")
	}
	rest := page[i:]
	open := strings.Index(rest, "[")
	if open < 0 {
		return nil, errors.New("albumFiles array does not open")
	}
	end := strings.Index(rest, "</script>")
	if end < 0 || end < open {
		end = len(rest)
	}
	body := rest[open+1 : end]

	var out []bunkrFile
	for _, chunk := range splitJSObjects(body) {
		fields := parseJSFields(chunk)
		id := fields["id"]
		if id == "" {
			continue
		}
		f := bunkrFile{
			ID:       id,
			Name:     fields["original"],
			Slug:     fields["slug"],
			Ext:      fields["extension"],
			MimeType: fields["type"],
			Size:     -1,
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(fields["size"]), 10, 64); err == nil && n > 0 {
			f.Size = n
		}
		if f.Name == "" {
			// Without a name, slug + extension is a reasonable fallback; better
			// than skipping an unnamed item.
			f.Name = strings.TrimSuffix(f.Slug, ".") + f.Ext
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, errors.New("no items found in albumFiles")
	}
	return out, nil
}

// splitJSObjects separates "{...}," blocks by brace balance.
// It doesn't trust a line-ending pattern ("\n},\n"): if formatting changes,
// that pattern silently produces one giant item.
func splitJSObjects(body string) []string {
	var out []string
	depth, start := 0, -1
	inStr := false
	var quote rune
	esc := false
	for i, r := range body {
		if inStr {
			switch {
			case esc:
				esc = false
			case r == '\\':
				esc = true
			case r == quote:
				inStr = false
			}
			continue
		}
		switch r {
		case '"', '\'':
			inStr, quote = true, r
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			depth--
			if depth == 0 && start >= 0 {
				out = append(out, body[start:i+1])
				start = -1
			}
		}
	}
	return out
}

// parseJSFields turns a JS object block into a "key -> value" map.
func parseJSFields(chunk string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(chunk, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "{")
		line = strings.TrimSuffix(line, "}")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.Trim(strings.TrimSpace(key), `"'`)
		val = strings.TrimSpace(val)
		val = strings.TrimSuffix(val, ",")
		val = strings.TrimSpace(val)
		if key == "" || val == "" {
			continue
		}
		out[key] = unquoteJS(val)
	}
	return out
}

// unquoteJS unquotes a quoted JS value. If there are JSON escapes it
// decodes them, otherwise it settles for trimming the quotes.
func unquoteJS(v string) string {
	if len(v) < 2 {
		return v
	}
	first, last := v[0], v[len(v)-1]
	if first == '"' && last == '"' {
		var s string
		if json.Unmarshal([]byte(v), &s) == nil {
			return s
		}
		return strings.Trim(v, `"`)
	}
	if first == '\'' && last == '\'' {
		inner := v[1 : len(v)-1]
		return strings.ReplaceAll(inner, `\'`, `'`)
	}
	return v
}

// ---------- API and decryption ----------

// bunkrAPIResponse is the download API's response. BOTH shapes are handled.
//
// The current shape (dl.bunkr.cr) gives the URL in pieces: mediafiles + path.
// The old shape (apidl.bunkr.ru) gives a full URL encrypted with XOR and in
// the 2026-09-10 measurement returned a STALE path: for the same file it said
// ".../file.m4v" where the current API says ".../storage/media/...". The old
// branch stays only for backward compatibility.
type bunkrAPIResponse struct {
	MediaFiles string `json:"mediafiles"`
	Path       string `json:"path"`
	Original   string `json:"original"`

	URL       string `json:"url"`
	Encrypted bool   `json:"encrypted"`
	Timestamp int64  `json:"timestamp"`
}

// bunkrSignResponse is the signing service's response.
type bunkrSignResponse struct {
	Token string `json:"token"`
	Ex    int64  `json:"ex"`
}

// redactToken deletes the token from a signing response before it is written
// to disk with --record.
//
// The token is a time-limited credential granting access to the file, and
// recordings are written with 0644. Diagnosis needs the SHAPE of the
// response, not the value: keeping the length is enough to answer "did a
// token arrive, is it plausible".
func redactToken(body []byte) []byte {
	var sig bunkrSignResponse
	if err := json.Unmarshal(body, &sig); err != nil {
		// If it can't be parsed it isn't in the expected shape; rather than
		// assuming it contains a token and hiding it all, we pass the raw body
		// through: error bodies (HTML, plain text) are valuable for diagnosis.
		return body
	}
	out, err := json.Marshal(struct {
		TokenLen int   `json:"token_len"`
		Ex       int64 `json:"ex"`
	}{len(sig.Token), sig.Ex})
	if err != nil {
		return nil
	}
	return out
}

// resolveFileURL resolves the real download URL for a data id.
//
// Referer and Origin are MANDATORY: the endpoint checks them and rejects the
// request if they are missing. gallery-dl's 2025-02-27 commit added both
// together. The SIGNATURE IS NOT TAKEN HERE. The URL is returned raw; the
// signature is fetched with PrepareURL when the download starts.
//
// Why: the token lives 2 hours (measured) but every item of an album would be
// signed at resolution time, while downloading only runs a few files at a
// time. On a slow connection the token of the file at the end of the queue
// dies before its turn and recovery costs three requests. Signing files that
// are skipped (already downloaded) or only listed was pure waste, too.
func (b *bunkr) resolveFileURL(ctx context.Context, dataID string) (string, string, error) {
	referer := b.dlOrigin + "/file/" + url.PathEscape(dataID)

	// If the primary endpoint is marked dead it is NOT TRIED until
	// primaryRetryAfter passes. Trying meant waiting a separate timeout for
	// every file of an album: on a 40-file album the UI froze for minutes.
	primaryErr := error(nil)
	if !b.primaryIsDead() {
		data, err := b.callAPI(ctx, b.apiEndpoint, dataID, referer)
		if err == nil {
			rawURL, rerr := b.rawFileURL(data, dataID)
			if rerr != nil {
				return "", "", rerr
			}
			return rawURL, referer, nil
		}
		// Fall back ONLY when the primary endpoint is UNREACHABLE. Real
		// answers such as "file deleted" (400) aren't fixed by the fallback.
		if !b.markPrimaryDead(err) {
			return "", "", err
		}
		primaryErr = err
	}

	fb, ferr := b.callAPI(ctx, b.fallbackAPI, dataID, referer)
	if ferr != nil {
		// The primary error is more informative: the real problem is that it
		// can't be reached.
		if primaryErr != nil {
			return "", "", primaryErr
		}
		return "", "", ferr
	}
	raw, rerr := b.rawFileURL(fb, dataID)
	if rerr != nil {
		return "", "", rerr
	}
	fixed, rerr := b.applyLegacyPrefix(raw, dataID)
	if rerr != nil {
		return "", "", rerr
	}
	return fixed, referer, nil
}

// primaryIsDead reports whether the primary endpoint is eliminated. The
// elimination lifts by itself after primaryRetryAfter: the primary is tried
// again, and if it is still unreachable markPrimaryDead eliminates it for
// another while.
func (b *bunkr) primaryIsDead() bool {
	if b.fallbackAPI == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.primaryDeadAt.IsZero() {
		return false
	}
	if b.now().Sub(b.primaryDeadAt) >= primaryRetryAfter {
		b.primaryDeadAt = time.Time{}
		b.cfg.Logln("bunkr: retrying endpoint %s", b.apiEndpoint)
		return false
	}
	return true
}

// callAPI sends a single POST to the download API.
func (b *bunkr) callAPI(ctx context.Context, endpoint, dataID, referer string) (bunkrAPIResponse, error) {
	var zero bunkrAPIResponse
	if endpoint == "" {
		return zero, Errorf(LayerItemPage, dataID, "no API endpoint defined")
	}

	payload, err := json.Marshal(map[string]string{"id": dataID})
	if err != nil {
		return zero, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return zero, Errorf(LayerItemPage, endpoint, "could not build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", referer)
	req.Header.Set("Origin", b.dlOrigin)
	if b.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", b.cfg.UserAgent)
	}

	resp, err := b.client().Do(req)
	if err != nil {
		return zero, &LayerError{Layer: LayerItemPage, Err: unwrapURLError(err), Evidence: endpoint}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusBadRequest {
		// gallery-dl interprets this as "album deleted".
		return zero, Errorf(LayerItemPage, dataID, "API 400: the album or file may have been deleted")
	}
	if resp.StatusCode != http.StatusOK {
		return zero, Errorf(LayerItemPage, endpoint, "API HTTP %s", resp.Status)
	}

	var data bunkrAPIResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return zero, Errorf(LayerItemPage, endpoint, "API response is not JSON: %v", err)
	}
	return data, nil
}

// markPrimaryDead decides whether an error on the primary endpoint requires
// the fallback and, if so, eliminates the endpoint for primaryRetryAfter.
//
// MEASUREMENT 2026-09-10: on this network dl.bunkr.cr is hijacked at the DNS
// level (195.175.254.2 = the ISP's block page, nothing answers on 443).
func (b *bunkr) markPrimaryDead(err error) bool {
	if b.fallbackAPI == "" {
		return false
	}
	// Only CONNECTION errors trigger the fallback; an answer the server gave
	// (400, 404, 500) isn't fixed by the fallback.
	if _, ok := burnReason(err); !ok {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.primaryDeadAt.IsZero() {
		b.primaryDeadAt = b.now()
		b.cfg.Logln("bunkr: endpoint %s unreachable (%v), using the fallback endpoint for %s: %s",
			b.apiEndpoint, unwrapURLError(err), FormatWait(primaryRetryAfter), b.fallbackAPI)
	}
	return true
}

// applyLegacyPrefix adds the storage prefix to the path given by the old endpoint.
//
// For the same file the old endpoint says ".../file.m4v" while the current
// one says ".../storage/media/file.m4v". The prefix was verified by
// MEASUREMENT: on 2026-09-10 it was signed and got 206 on three different
// CDN nodes across two albums. It is still a GUESS; when the current endpoint
// is reachable it tells the real path, so the prefix is only used on the
// fallback path.
func (b *bunkr) applyLegacyPrefix(raw, dataID string) (string, error) {
	if b.legacyPrefix == "" {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", Errorf(LayerItemPage, dataID, "could not parse the old endpoint's URL: %v", err)
	}
	if strings.HasPrefix(u.Path, b.legacyPrefix+"/") {
		return raw, nil
	}
	u.Path = b.legacyPrefix + u.Path
	u.RawPath = ""
	return u.String(), nil
}

// PrepareURL signs the URL right as the download starts. site.URLPreparer.
//
// Because it is called again on every attempt, an expired token refreshes
// itself: there is no need to get a 403 and resolve the item from scratch.
func (b *bunkr) PrepareURL(ctx context.Context, rawURL string) (string, error) {
	return b.signURL(ctx, rawURL)
}

// rawFileURL builds the raw URL to be signed from the API response.
func (b *bunkr) rawFileURL(data bunkrAPIResponse, dataID string) (string, error) {
	if data.MediaFiles != "" && data.Path != "" {
		// Base and path are NOT concatenated and then parsed. If the file name
		// contains '#' or '?', url.Parse takes them for a fragment/query and
		// CUTS the path: "track #3.mp4" -> Path="/…/track ", Fragment="3.mp4".
		// Then the signature is taken for the wrong path and the CDN returns
		// 403 — the hardest error class to diagnose, because it looks like
		// "the signature expired".
		//
		// Parsing is applied only to the base; the path is assigned as a FIELD
		// and String() does the escaping. This also makes it impossible for a
		// path like "@evil.tld/x" to change the host during concatenation.
		u, err := url.Parse(strings.TrimRight(data.MediaFiles, "/"))
		if err != nil {
			return "", Errorf(LayerItemPage, dataID, "could not parse the API URL: %v", err)
		}
		if u.Scheme != "https" || u.Host == "" {
			return "", Errorf(LayerItemPage, dataID,
				"API returned an unexpected download base: %q", data.MediaFiles)
		}
		p := data.Path
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		u.Path = p
		u.RawPath = "" // let the escaping be derived from Path again

		// n is the original name the CDN uses in Content-Disposition. The site
		// itself adds it BEFORE signing too; the signature only looks at the
		// path, so the order doesn't change the result.
		if data.Original != "" {
			q := u.Query()
			q.Set("n", data.Original)
			u.RawQuery = q.Encode()
		}
		return u.String(), nil
	}

	if data.URL == "" {
		return "", Errorf(LayerItemPage, dataID, "API gave neither mediafiles/path nor url")
	}
	if !data.Encrypted {
		return data.URL, nil
	}
	key := b.xorPrefix + strconv.FormatInt(data.Timestamp/3600, 10)
	dec, err := decryptXOR(data.URL, []byte(key))
	if err != nil {
		return "", Errorf(LayerItemPage, dataID, "could not decrypt the URL: %v", err)
	}
	return dec, nil
}

// signURL signs the CDN URL with a token from the signing service.
//
// Why a separate service: the CDN rejects an unsigned GET without looking at
// the file. 2026-09-10 measurement: for an existing file and a MADE-UP file
// name the response is byte-for-byte identical (403, same body, same
// headers). So looking at a 403 and saying "the file was deleted" would be WRONG.
//
// The token is time-limited (the ex field). When it expires the downloader
// re-resolves the item with ResolveOne; that is why Item.SourcePage is mandatory.
func (b *bunkr) signURL(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", Errorf(LayerCDN, rawURL, "could not parse the URL: %v", err)
	}

	// The service gets the DECODED path: the site's JS applies
	// decodeURIComponent and re-encodes with encodeURIComponent. u.Path is
	// already decoded, and QueryEscape encodes everything including "/".
	endpoint := b.signEndpoint + "?path=" + url.QueryEscape(u.Path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", Errorf(LayerCDN, endpoint, "could not build request: %v", err)
	}
	if b.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", b.cfg.UserAgent)
	}

	resp, err := b.client().Do(req)
	if err != nil {
		return "", &LayerError{Layer: LayerCDN, Err: unwrapURLError(err), Evidence: endpoint}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	b.cfg.Recordln("sign.json", redactToken(body))

	if resp.StatusCode != http.StatusOK {
		// A challenge is classified SEPARATELY: saying "signing service 403"
		// would send the user to the config, while the problem is Cloudflare.
		if resp.StatusCode == http.StatusForbidden && looksLikeChallenge(resp, body) {
			return "", &LayerError{
				Layer:    LayerChallenge,
				Err:      errors.New("the signing service returned a Cloudflare challenge"),
				Evidence: endpoint,
			}
		}
		return "", Errorf(LayerCDN, endpoint, "signing service HTTP %s", resp.Status)
	}
	var sig bunkrSignResponse
	if err := json.Unmarshal(body, &sig); err != nil {
		return "", Errorf(LayerCDN, endpoint, "signing response is not JSON: %v", err)
	}
	if sig.Token == "" {
		return "", Errorf(LayerCDN, endpoint, "no token in the signing response")
	}
	// ex is CHECKED too: if it is missing 0 is written, the CDN returns a
	// plain 403 and ClassifyStatus counts it as "signature expired". Result:
	// the same broken URL is built once more and retried, and the user sees
	// the wrong error.
	if sig.Ex <= 0 {
		return "", Errorf(LayerCDN, endpoint, "no valid ex in the signing response")
	}

	q := u.Query()
	q.Set("token", sig.Token)
	q.Set("ex", strconv.FormatInt(sig.Ex, 10))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// decryptXOR decodes base64 data and XORs it with the key.
//
// The key is time-based (timestamp/3600), so the decrypted URL is valid in an
// hourly window. This is bunkr's reason for Item.SourcePage being mandatory:
// a resume the next day gets 403 on the old URL and the item has to be
// re-resolved.
func decryptXOR(b64 string, key []byte) (string, error) {
	if len(key) == 0 {
		return "", errors.New("empty key")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", fmt.Errorf("could not decode base64: %w", err)
	}
	out := make([]byte, len(raw))
	for i := range raw {
		out[i] = raw[i] ^ key[i%len(key)]
	}
	return string(out), nil
}

// ---------- Resolver interface ----------

func (b *bunkr) Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error) {
	ref, err := b.parse(u)
	if err != nil {
		return nil, Errorf(LayerParse, u, "%v", err)
	}

	if ref.kind == bunkrMedia {
		item, err := b.resolveMedia(ctx, "/"+ref.seg+"/"+ref.id)
		if err != nil {
			return nil, err
		}
		if err := yield(item); err != nil {
			return nil, err
		}
		return nil, nil
	}

	// advanced=1 is MANDATORY: window.albumFiles only comes with this
	// parameter. The old way (scraping grid-images_box divs) missed everything
	// after 100 files; gallery-dl changed its data source for exactly this
	// reason on 2025-08-31. So it wasn't a pagination problem, it was a source
	// problem.
	page, root, err := b.fetchWithRotation(ctx, "/a/"+url.PathEscape(ref.id)+"?advanced=1", "")
	if err != nil {
		return nil, err
	}
	text := string(page)

	files, err := parseAlbumFiles(text)
	if err != nil {
		return nil, Errorf(LayerParse, root+"/a/"+ref.id, "%v", err)
	}

	dir := sanitizeDirLabel(ogTitle(text), ref.id)

	var itemErrs []ItemError
	for i, f := range files {
		fileURL, referer, ferr := b.resolveFileURL(ctx, f.ID)
		if ferr != nil {
			itemErrs = append(itemErrs, ItemError{
				URL: root + "/f/" + f.Slug,
				Err: ferr,
			})
			continue
		}
		item := Item{
			URL:        fileURL,
			SourcePage: root + "/f/" + f.Slug,
			Headers:    map[string]string{"Referer": referer},
			Dir:        dir,
			Filename:   f.Name,
			Size:       f.Size,
			Index:      i,
			// bunkr doesn't give sha256; resume relies on ETag/Last-Modified.
		}
		if err := yield(item); err != nil {
			return itemErrs, err
		}
	}
	return itemErrs, nil
}

// resolveMedia produces an Item from a single media page.
func (b *bunkr) resolveMedia(ctx context.Context, path string) (Item, error) {
	page, root, err := b.fetchWithRotation(ctx, path, "")
	if err != nil {
		return Item{}, err
	}
	text := string(page)

	dataID := extractBetween(text, `data-file-id="`, `"`)
	if dataID == "" {
		return Item{}, Errorf(LayerParse, root+path, "data-file-id not found")
	}
	fileURL, referer, err := b.resolveFileURL(ctx, dataID)
	if err != nil {
		return Item{}, err
	}

	name := strings.TrimSpace(ogTitle(text))
	if name == "" {
		name = strings.TrimPrefix(path[strings.LastIndex(path, "/"):], "/")
	}
	return Item{
		URL:        fileURL,
		SourcePage: root + path,
		Headers:    map[string]string{"Referer": referer},
		Filename:   name,
		Size:       -1,
		Index:      0,
	}, nil
}

// ResolveOne re-resolves a single item when the signed URL expired.
// On bunkr this path is vital: the XOR key is tied to an hourly window.
func (b *bunkr) ResolveOne(ctx context.Context, sourcePage string) (Item, error) {
	ref, err := b.parse(sourcePage)
	if err != nil {
		return Item{}, Errorf(LayerParse, sourcePage, "%v", err)
	}
	if ref.kind != bunkrMedia {
		return Item{}, Errorf(LayerParse, sourcePage, "expected an item page, got an album")
	}
	return b.resolveMedia(ctx, "/"+ref.seg+"/"+ref.id)
}

// ClassifyStatus lets the downloader interpret 403/410 correctly.
func (b *bunkr) ClassifyStatus(resp *http.Response, body []byte) error {
	if resp.StatusCode != http.StatusForbidden {
		return nil
	}
	if looksLikeChallenge(resp, body) {
		evidence := ""
		if resp.Request != nil && resp.Request.URL != nil {
			evidence = resp.Request.URL.String()
		}
		return &LayerError{
			Layer:    LayerChallenge,
			Err:      errors.New("the CDN returned a Cloudflare challenge"),
			Evidence: evidence,
		}
	}
	// Not a challenge: the signed URL may have expired; let the downloader
	// re-resolve with ResolveOne.
	return nil
}

// maintenanceNames are the placeholder files bunkr serves in maintenance mode.
var maintenanceNames = []string{"/maint.mp4", "/maintenance-vid.mp4"}

// ValidateResponse catches the maintenance placeholder.
//
// For deleted or maintenance files bunkr returns a placeholder video with
// 200, NOT 404. The status is clean, the content is garbage. Without this
// check the tool counts the garbage as "downloaded successfully"; the worst
// kind of silent corruption. gallery-dl had to add the same detection in two
// separate commits.
func (b *bunkr) ValidateResponse(resp *http.Response) error {
	final := ""
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.Path
	}
	for _, name := range maintenanceNames {
		if strings.HasSuffix(final, name) {
			return Errorf(LayerCDN, final,
				"the file server is in maintenance mode: a placeholder video came back, not the real content")
		}
	}
	return nil
}

func (b *bunkr) Diagnose(ctx context.Context) ([]LayerResult, error) {
	if len(b.cfg.CanaryURLs) == 0 {
		return nil, errors.New("canary URL list is empty")
	}
	var last []LayerResult
	for _, canary := range b.cfg.CanaryURLs {
		res := b.diagnoseOne(ctx, canary)
		last = res
		if !hasFail(res) {
			return res, nil
		}
	}
	return last, nil
}

func (b *bunkr) diagnoseOne(ctx context.Context, canary string) []LayerResult {
	out := make([]LayerResult, 0, 6)

	u, err := url.Parse(canary)
	if err != nil {
		return append(out, LayerResult{Layer: LayerDNS, Status: StatusFail,
			Detail: "could not parse the canary URL", Evidence: canary})
	}
	host := normalizeHost(u.Host)

	addrs, dnsErr := net.DefaultResolver.LookupIPAddr(ctx, host)
	if dnsErr != nil {
		return append(out, LayerResult{Layer: LayerDNS, Status: StatusFail,
			Detail: "did not resolve", Evidence: host + ": " + dnsErr.Error()})
	}
	ips := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP.String())
	}
	out = append(out, LayerResult{Layer: LayerDNS, Status: StatusOK,
		Detail: fmt.Sprintf("%d addresses", len(ips)), Evidence: strings.Join(ips, ", ")})

	body, err := b.get(ctx, canary, "")
	// The response is recorded in every case: the broken page's body is the
	// real evidence, but a working body is the reference for a future diff.
	b.cfg.Recordln("canary.html", body)

	switch {
	case err == nil:
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "handshake OK"},
			LayerResult{Layer: LayerChallenge, Status: StatusOK, Detail: "no challenge"},
			LayerResult{Layer: LayerFetch, Status: StatusOK,
				Detail: fmt.Sprintf("200, %d KB", len(body)/1024)},
		)
		// The Parse layer can only be verified if the canary is an ALBUM
		// page. The site root carries no albumFiles; calling that a FAIL makes
		// doctor a liar and sends the user in the wrong direction ("parse broke").
		if !strings.Contains(u.Path, "/a/") {
			out = append(out,
				LayerResult{Layer: LayerParse, Status: StatusWarn,
					Detail:   "no album canary, parsing not verified",
					Evidence: "add an /a/<id> URL to canary_urls"},
				LayerResult{Layer: LayerItemPage, Status: StatusWarn,
					Detail: "no album canary, API chain not verified"},
				LayerResult{Layer: LayerCDN, Status: StatusWarn,
					Detail: "no album canary, no CDN host seen"},
			)
		} else if files, perr := parseAlbumFiles(string(body)); perr != nil {
			out = append(out, LayerResult{Layer: LayerParse, Status: StatusFail,
				Detail: "could not parse albumFiles", Evidence: perr.Error()})
		} else {
			out = append(out, LayerResult{Layer: LayerParse, Status: StatusOK,
				Detail: fmt.Sprintf("albumFiles found %d items", len(files))})
			out = append(out, b.diagnoseItemAndCDN(ctx, files)...)
		}

	default:
		reason, burnable := burnReason(err)
		layer := LayerFetch
		if l, ok := LayerOf(err); ok {
			layer = l
		}
		var ch *challengeError
		switch {
		case errors.As(err, &ch):
			layer = LayerChallenge
		case burnable && strings.Contains(reason, "certificate"):
			layer = LayerTLS
		case burnable && strings.Contains(reason, "timeout"):
			layer = LayerTLS
		}
		detail := "request failed"
		if burnable {
			detail = reason
		}
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "DNS passed"},
			LayerResult{Layer: layer, Status: StatusFail, Detail: detail, Evidence: err.Error()},
		)
	}
	return out
}

// diagnoseItemAndCDN verifies the API chain and the CDN host when there is an
// album canary.
//
// A SINGLE item is resolved. doctor is a diagnostic tool; firing 200 API
// calls for 200 items would turn diagnosis into punishment and trigger the
// rate limit with our own hands.
func (b *bunkr) diagnoseItemAndCDN(ctx context.Context, files []bunkrFile) []LayerResult {
	var out []LayerResult

	fileURL, _, err := b.resolveFileURL(ctx, files[0].ID)
	if err != nil {
		// The error's OWN layer is kept. Writing every failure to ItemPage
		// sent the diagnosis to the wrong place: even when the signing
		// service crashed, the user started fiddling with api_endpoint.
		layer := LayerItemPage
		if l, ok := LayerOf(err); ok {
			layer = l
		}
		fail := LayerResult{Layer: layer, Status: StatusFail,
			Detail: "API chain broken", Evidence: collapseSpace(err.Error())}
		if layer == LayerCDN || layer == LayerChallenge {
			return append(out, fail)
		}
		return append(out, fail,
			LayerResult{Layer: LayerCDN, Status: StatusWarn,
				Detail: "the previous layer broke, so no CDN host could be seen"},
		)
	}
	out = append(out, LayerResult{Layer: LayerItemPage, Status: StatusOK,
		Detail: fmt.Sprintf("1/%d items resolved (sampling)", len(files))})

	host := ""
	if u, perr := url.Parse(fileURL); perr == nil {
		host = u.Host
	}
	switch {
	case host == "":
		out = append(out, LayerResult{Layer: LayerCDN, Status: StatusFail,
			Detail: "no host in the resolved URL", Evidence: fileURL})
	case MatchHost(host, b.cfg.CDNPatterns):
		out = append(out, LayerResult{Layer: LayerCDN, Status: StatusOK,
			Detail: "known CDN host", Evidence: host})
	default:
		// The CDN is a SIGNAL, NOT A GATE. An unknown host doesn't stop the
		// download; bunkr hosts rotate in normal operation. Making it a gate
		// would be producing the very breakage the tool later diagnoses.
		out = append(out, LayerResult{Layer: LayerCDN, Status: StatusWarn,
			Detail:   "new CDN host, not in cdn_patterns",
			Evidence: host + " (add it to sites.toml)"})
	}
	return out
}

// ogTitle returns the page's og:title value. The value is an HTML attribute,
// so entities ("&amp;", "&#39;") are decoded; otherwise they ended up in
// folder and file names as is.
func ogTitle(page string) string {
	return html.UnescapeString(extractBetween(page, `property="og:title" content="`, `"`))
}

// extractBetween returns the first start...end span; "" if not found.
func extractBetween(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		return ""
	}
	return s[:j]
}

// ordinal formats which attempt succeeded, for the log.
func ordinal(i int) string {
	if i == 0 {
		return " (first try)"
	}
	return fmt.Sprintf(" (attempt %d)", i+1)
}
