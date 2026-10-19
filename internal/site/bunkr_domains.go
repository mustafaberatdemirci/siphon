package site

// bunkr: rotating over its domains, and the HTTP requests made that way.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

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

// ordinal formats which attempt succeeded, for the log.
func ordinal(i int) string {
	if i == 0 {
		return " (first try)"
	}
	return fmt.Sprintf(" (attempt %d)", i+1)
}
