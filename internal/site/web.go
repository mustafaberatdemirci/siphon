package site

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// web is the request plumbing shared by the resolvers that read a site's
// JSON API and pages (mediafire, gofile, cyberdrop): the site's User-Agent,
// a capped body, and transport failures sorted into the layers doctor
// reports, the way pixeldrain and bunkr do it.
type web struct{ cfg SiteConfig }

func (w web) client() *http.Client {
	if w.cfg.HTTPClient != nil {
		return w.cfg.HTTPClient
	}
	return http.DefaultClient
}

// maxBody caps what is read of an API answer or a page. mediafire's file
// page is about 100 KB, a 1,000-entry folder listing about 1 MB.
const maxBody = 16 << 20

// do sends req and returns the response (its body already read and closed)
// with the body, whatever the status: these APIs explain their errors in the
// body of a non-200 answer, so judging the status is the caller's job. Only
// a transport failure or a Cloudflare challenge is an error here.
func (w web) do(req *http.Request) (*http.Response, []byte, error) {
	if w.cfg.UserAgent != "" && req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", w.cfg.UserAgent)
	}
	raw := req.URL.String()
	resp, err := w.client().Do(req)
	if err != nil {
		return nil, nil, classifyTransportError(raw, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, nil, Errorf(LayerFetch, raw, "could not read body: %v", err)
	}
	if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusServiceUnavailable) &&
		looksLikeChallenge(resp, body) {
		return resp, body, Errorf(LayerChallenge, raw, "Cloudflare challenge (HTTP %d)", resp.StatusCode)
	}
	return resp, body, nil
}

// get is do for a GET with extra headers.
func (w web) get(ctx context.Context, rawURL string, headers map[string]string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, Errorf(LayerFetch, rawURL, "could not build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return w.do(req)
}

// HTTPError is an answer whose status the resolver has nothing better to
// say about.
type HTTPError struct {
	Status  int
	Snippet string // the start of the body, for the evidence
}

func (e *HTTPError) Error() string {
	if e.Snippet == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Snippet)
}

// statusError describes an unexpected status.
func statusError(rawURL string, resp *http.Response, body []byte) error {
	snippet := strings.Join(strings.Fields(string(body[:min(len(body), 200)])), " ")
	return &LayerError{Layer: LayerFetch, Err: &HTTPError{Status: resp.StatusCode, Snippet: snippet}, Evidence: rawURL}
}

// splitPath returns the non-empty segments of a URL path.
func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseLink parses a pasted link (the scheme may be missing) and returns it
// with its normalized host.
func parseLink(raw string) (*url.URL, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, "", errors.New("empty URL")
	}
	if !strings.Contains(raw, "//") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", fmt.Errorf("could not parse URL: %w", err)
	}
	return u, normalizeHost(u.Host), nil
}

// diagnoseCanaries runs doctor's checks against each canary in turn and
// stops at the first that passes: the DNS lookup of the canary's host, then
// check, which does the site's own requests. check returns the layers it
// verified and, if one failed, an error carrying that layer; TLS and the
// challenge are inferred from how far it got.
func diagnoseCanaries(ctx context.Context, cfg SiteConfig, check func(ctx context.Context, canary string) ([]LayerResult, error)) ([]LayerResult, error) {
	if len(cfg.CanaryURLs) == 0 {
		return nil, errors.New("canary URL list is empty")
	}
	var last []LayerResult
	for _, canary := range cfg.CanaryURLs {
		last = diagnoseCanary(ctx, canary, check)
		if !hasFail(last) {
			return last, nil
		}
	}
	return last, nil
}

func diagnoseCanary(ctx context.Context, canary string, check func(ctx context.Context, canary string) ([]LayerResult, error)) []LayerResult {
	u, err := url.Parse(canary)
	if err != nil || u.Host == "" {
		return []LayerResult{{Layer: LayerDNS, Status: StatusFail, Detail: "could not parse the canary URL", Evidence: canary}}
	}
	host := normalizeHost(u.Host)
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return []LayerResult{{Layer: LayerDNS, Status: StatusFail, Detail: "did not resolve", Evidence: host + ": " + err.Error()}}
	}
	ips := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP.String())
	}
	out := []LayerResult{{Layer: LayerDNS, Status: StatusOK, Detail: fmt.Sprintf("%d addresses", len(ips)), Evidence: strings.Join(ips, ", ")}}

	passed, err := check(ctx, canary)
	layer, ok := LayerOf(err)
	if err != nil && !ok {
		layer = LayerFetch
	}
	if err == nil || (layer != LayerDNS && layer != LayerTLS) {
		out = append(out, LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "handshake OK"})
	}
	if err == nil || (layer != LayerDNS && layer != LayerTLS && layer != LayerChallenge) {
		out = append(out, LayerResult{Layer: LayerChallenge, Status: StatusOK, Detail: "no challenge"})
	}
	out = append(out, passed...)
	if err != nil {
		out = append(out, LayerResult{Layer: layer, Status: StatusFail, Detail: "failed", Evidence: err.Error()})
	}
	return out
}

// cdnResult is doctor's CDN line for a download host: a signal, not a gate.
func cdnResult(cfg SiteConfig, rawURL string) LayerResult {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return LayerResult{Layer: LayerCDN, Status: StatusWarn, Detail: "no download host to check", Evidence: rawURL}
	}
	host := normalizeHost(u.Host)
	if len(cfg.CDNPatterns) == 0 || MatchHost(host, cfg.CDNPatterns) {
		return LayerResult{Layer: LayerCDN, Status: StatusOK, Detail: "download host " + host}
	}
	return LayerResult{Layer: LayerCDN, Status: StatusWarn, Detail: "new download host, add it to cdn_patterns in sites.toml", Evidence: host}
}

// transientError marks a failure worth another attempt after a pause. The
// retry policy reads Retryable through an interface, so site doesn't have
// to import it.
type transientError struct{ error }

func (e transientError) Retryable() bool { return true }
func (e transientError) Unwrap() error   { return e.error }

// captchaError says the site wants a human to solve a captcha: waiting
// doesn't help and the queue stops asking that site until the user resumes.
// Solving captchas is out of scope.
type captchaError struct{ error }

func (e captchaError) CaptchaRequired() bool { return true }
func (e captchaError) Unwrap() error         { return e.error }

// isPage reports whether an answer is a web page.
func isPage(resp *http.Response) bool {
	return isPageType(resp.Header.Get("Content-Type"))
}

// pageInsteadOfFile reports a download answered with a web page from a host
// that is not a download server: a redirect to the site, which is how an
// expired link or session ends (measured on gofile). A page from the
// download server itself may be the file: anyone can upload an .html.
func pageInsteadOfFile(resp *http.Response, cdnPatterns []string) bool {
	if !isPage(resp) {
		return false
	}
	if resp.Request == nil || resp.Request.URL == nil {
		return true
	}
	return !MatchHost(resp.Request.URL.Host, cdnPatterns)
}
