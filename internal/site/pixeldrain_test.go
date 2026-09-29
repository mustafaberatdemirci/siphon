package site

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testCfg() SiteConfig {
	return SiteConfig{
		Name:          PixeldrainName,
		Domains:       []string{"pixeldrain.com", "pixeldra.in", "pixeldrain.net", "pixeldrain.nl", "pixeldrain.dev", "pixeldrain.biz", "pixeldrain.tech"},
		LegacyDomains: []string{"pixeldrain.old"},
		RefererPolicy: RefererNone,
		CanaryURLs:    []string{"https://pixeldrain.com/api/misc/rate_limits"},
	}.WithDefaults()
}

func TestMatchHost(t *testing.T) {
	cases := []struct {
		host     string
		patterns []string
		want     bool
	}{
		{"pixeldrain.com", []string{"pixeldrain.com"}, true},
		{"PixelDrain.COM", []string{"pixeldrain.com"}, true},
		{"pixeldrain.com.", []string{"pixeldrain.com"}, true},
		{"pixeldrain.com:443", []string{"pixeldrain.com"}, true},
		{"evil.com", []string{"pixeldrain.com"}, false},
		{"", []string{"pixeldrain.com"}, false},

		// Wildcard: gallery-dl and cyberdrop-dl use "bunkr.*" for bunkr.
		{"bunkr.cr", []string{"bunkr.*"}, true},
		{"bunkr.ws", []string{"bunkr.*"}, true},
		{"notbunkr.cr", []string{"bunkr.*"}, false},
		{"bunkr.", []string{"bunkr.*"}, false},
		{"bunkr", []string{"bunkr.*"}, false},

		// SECURITY BOUNDARY: a wildcard matches a SINGLE label. If it could
		// span labels, a hostile link in the input list would be treated as a
		// trusted site.
		{"bunkr.attacker.com", []string{"bunkr.*"}, false},
		{"bunkr.evil.example.org", []string{"bunkr.*"}, false},
		{"bunkr.com.phish.ru", []string{"bunkr.*"}, false},
		// The price: a multi-part TLD must be listed explicitly.
		{"bunkr.co.uk", []string{"bunkr.*"}, false},
		{"bunkr.co.uk", []string{"bunkr.co.uk"}, true},

		{"cdn.bunkr.la", []string{"*.bunkr.la"}, true},
		{"a.b.bunkr.la", []string{"*.bunkr.la"}, false},
		{"bunkr.la", []string{"*.bunkr.la"}, false},

		// A scheme/path given in the pattern is stripped.
		{"pixeldrain.com", []string{"https://pixeldrain.com/"}, true},
	}
	for _, c := range cases {
		if got := MatchHost(c.host, c.patterns); got != c.want {
			t.Errorf("MatchHost(%q, %v) = %v, want %v", c.host, c.patterns, got, c.want)
		}
	}
}

func TestParseURLForms(t *testing.T) {
	p := &pixeldrain{cfg: testCfg()}
	cases := []struct {
		in       string
		wantKind refKind
		wantID   string
		wantErr  bool
	}{
		{"https://pixeldrain.com/l/abc123", refAlbum, "abc123", false},
		{"https://pixeldrain.com/u/xyz789", refFile, "xyz789", false},
		{"https://pixeldrain.com/api/list/abc123", refAlbum, "abc123", false},
		{"https://pixeldrain.com/api/file/xyz789", refFile, "xyz789", false},
		{"https://pixeldrain.com/api/file/xyz789/info", refFile, "xyz789", false},

		// Short link: pixeldra.in/{id}
		{"https://pixeldra.in/qwerty", refFile, "qwerty", false},
		{"pixeldra.in/qwerty", refFile, "qwerty", false},

		// Alternative TLDs; all seven were measured live.
		{"https://pixeldrain.tech/l/abc123", refAlbum, "abc123", false},
		{"https://pixeldrain.biz/u/xyz789", refFile, "xyz789", false},

		// LegacyDomains are RECOGNIZED but not used for fetching (separate test below).
		{"https://pixeldrain.old/u/xyz789", refFile, "xyz789", false},

		{"https://evil.com/l/abc123", 0, "", true},
		{"https://pixeldrain.com/l/", 0, "", true},
		{"https://pixeldrain.com/", 0, "", true},
		{"", 0, "", true},
	}
	for _, c := range cases {
		got, err := p.parse(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parse(%q) expected an error, got %+v", c.in, got)
			}
			if p.Match(c.in) {
				t.Errorf("Match(%q) = true, want false", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parse(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got.kind != c.wantKind || got.id != c.wantID {
			t.Errorf("parse(%q) = {%v %q}, want {%v %q}", c.in, got.kind, got.id, c.wantKind, c.wantID)
		}
		if !p.Match(c.in) {
			t.Errorf("Match(%q) = false, want true", c.in)
		}
	}
}

func TestFetchHostSkipsLegacyDomain(t *testing.T) {
	p := &pixeldrain{cfg: testCfg()}
	r, err := p.parse("https://pixeldrain.old/u/xyz789")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	host, err := p.fetchHost(r)
	if err != nil {
		t.Fatalf("fetchHost: %v", err)
	}
	if host == "pixeldrain.old" {
		t.Fatal("a dead domain was used for fetching; the LegacyDomains distinction is not working")
	}
	if host != "pixeldrain.com" {
		t.Fatalf("fetchHost = %q, expected the first entry of the active list", host)
	}
}

// rewriteTransport redirects every request to the test server.
// This is the test injection mechanism the design names.
type rewriteTransport struct {
	target *url.URL
	seen   []string
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.seen = append(rt.seen, req.URL.Path)
	clone := req.Clone(req.Context())
	clone.URL.Scheme = rt.target.Scheme
	clone.URL.Host = rt.target.Host
	clone.Host = ""
	return http.DefaultTransport.RoundTrip(clone)
}

func newTestResolver(t *testing.T, h http.HandlerFunc) (*pixeldrain, *rewriteTransport) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("test server URL: %v", err)
	}
	rt := &rewriteTransport{target: u}
	cfg := testCfg()
	cfg.HTTPClient = &http.Client{Transport: rt}
	return &pixeldrain{cfg: cfg}, rt
}

const albumJSON = `{
  "success": true,
  "id": "abc123",
  "title": "Holiday 2026",
  "file_count": 3,
  "files": [
    {"success": true, "id": "f1", "name": "one.jpg",   "size": 100, "mime_type": "image/jpeg"},
    {"success": true, "id": "f2", "name": "two.mp4",   "size": 200, "mime_type": "video/mp4"},
    {"success": true, "id": "f3", "name": "three.png", "size": 0,   "mime_type": "image/png"}
  ]
}`

func TestResolveAlbumUsesSingleRequest(t *testing.T) {
	p, rt := newTestResolver(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/list/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(albumJSON))
	})

	var items []Item
	itemErrs, err := p.Resolve(context.Background(), "https://pixeldrain.com/l/abc123",
		func(it Item) error { items = append(items, it); return nil })
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(itemErrs) != 0 {
		t.Fatalf("unexpected item errors: %v", itemErrs)
	}
	if len(items) != 3 {
		t.Fatalf("%d items, want 3", len(items))
	}

	// The real criterion: the rule against 201 requests for a 200-file album.
	if len(rt.seen) != 1 {
		t.Fatalf("%d requests sent (%v), want 1; per-file /info calls trigger the rate limit",
			len(rt.seen), rt.seen)
	}

	if items[0].Dir != "Holiday 2026" {
		t.Errorf("Dir = %q", items[0].Dir)
	}
	if items[0].Filename != "one.jpg" || items[0].Index != 0 {
		t.Errorf("wrong first item: %+v", items[0])
	}
	if items[1].SourcePage != "https://pixeldrain.com/u/f2" {
		t.Errorf("SourcePage = %q; required for re-resolution", items[1].SourcePage)
	}
	if !strings.Contains(items[1].URL, "/api/file/f2") {
		t.Errorf("URL = %q", items[1].URL)
	}
	// An unknown size is -1, not 0.
	if items[2].Size != -1 {
		t.Errorf("unknown size = %d, want -1", items[2].Size)
	}
	// The real /list response carries hash_sha256 (verified live: 34/34).
	// The fixture has no such field, but when filled it must be carried into the Item.
	if items[0].SHA256 != "" {
		t.Errorf("the fixture has no hash, SHA256 should be empty: %q", items[0].SHA256)
	}
}

func TestResolveSingleFileHasSHA256(t *testing.T) {
	p, _ := newTestResolver(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"id":"xyz789","name":"single.bin","size":42,"hash_sha256":"deadbeef"}`))
	})
	var got Item
	if _, err := p.Resolve(context.Background(), "https://pixeldrain.com/u/xyz789",
		func(it Item) error { got = it; return nil }); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.SHA256 != "deadbeef" {
		t.Errorf("SHA256 = %q; must be filled on the single-file path", got.SHA256)
	}
	if got.Size != 42 || got.Filename != "single.bin" {
		t.Errorf("wrong item: %+v", got)
	}
}

func TestErrorEnvelopeMapsToAPIError(t *testing.T) {
	p, _ := newTestResolver(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"value":"transfer_limit_exceeded","message":"limit"}`))
	})
	_, err := p.Resolve(context.Background(), "https://pixeldrain.com/l/abc123", func(Item) error { return nil })
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T: %v", err, err)
	}
	if apiErr.Value != "transfer_limit_exceeded" {
		t.Errorf("Value = %q", apiErr.Value)
	}
	if !apiErr.Retryable() {
		t.Error("transfer_limit_exceeded must be retryable")
	}
	if apiErr.CaptchaRequired() {
		t.Error("transfer_limit_exceeded doesn't require a captcha")
	}
	if l, ok := LayerOf(err); !ok || l != LayerFetch {
		t.Errorf("Layer = %v, want %v", l, LayerFetch)
	}
}

func TestCaptchaCodeIsNotRetryable(t *testing.T) {
	e := &APIError{Status: 403, Value: "file_rate_limited_captcha_required"}
	if e.Retryable() {
		t.Error("a code requiring a captcha must not be retryable; waiting doesn't solve it")
	}
	if !e.CaptchaRequired() {
		t.Error("CaptchaRequired returned false")
	}
}

func TestChallengeIsItsOwnLayer(t *testing.T) {
	p, _ := newTestResolver(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("CF-Mitigated", "challenge")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<!DOCTYPE html><title>Just a moment...</title>`))
	})
	_, err := p.Resolve(context.Background(), "https://pixeldrain.com/l/abc123", func(Item) error { return nil })
	if err == nil {
		t.Fatal("expected an error")
	}
	l, ok := LayerOf(err)
	if !ok || l != LayerChallenge {
		t.Fatalf("Layer = %v, want %v; a challenge must be separate from TLS", l, LayerChallenge)
	}
}

func TestResolveOneRejectsAlbumURL(t *testing.T) {
	p := &pixeldrain{cfg: testCfg()}
	if _, err := p.ResolveOne(context.Background(), "https://pixeldrain.com/l/abc123"); err == nil {
		t.Fatal("expected an error for an album URL")
	}
}

func TestRegistryBuildsResolver(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(PixeldrainName, NewPixeldrain); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Register(PixeldrainName, NewPixeldrain); err == nil {
		t.Error("a second registration under the same name must fail")
	}
	rs, err := reg.Build([]SiteConfig{{Name: PixeldrainName, Domains: []string{"pixeldrain.com"}}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(rs) != 1 || !rs[0].Match("https://pixeldrain.com/l/abc") {
		t.Fatal("resolver was not built or Match didn't work")
	}
	if _, err := reg.Build([]SiteConfig{{Name: "missing"}}); err == nil {
		t.Error("expected an error for an unregistered site")
	}
}

func TestWithDefaults(t *testing.T) {
	c := SiteConfig{}.WithDefaults()
	if c.MaxRetries != DefaultMaxRetries || c.BaseDelay != DefaultBaseDelay ||
		c.MaxDelay != DefaultMaxDelay || c.MaxElapsed != DefaultMaxElapsed ||
		c.MaxConcurrent != DefaultMaxConcurrent || c.RefererPolicy != RefererNone {
		t.Fatalf("missing defaults: %+v", c)
	}
	// A given value must not be overwritten.
	c2 := SiteConfig{MaxRetries: 1, RefererPolicy: RefererItemPage}.WithDefaults()
	if c2.MaxRetries != 1 || c2.RefererPolicy != RefererItemPage {
		t.Fatalf("a given value was overwritten: %+v", c2)
	}
}

// The real pixeldrain /list response, unlike the official API docs, returns a
// filled hash_sha256 inside files[]. This test pins that behavior: a filled
// hash must be carried into the Item and verified by the downloader.
func TestAlbumCarriesSHA256WhenPresent(t *testing.T) {
	const withHash = `{"success":true,"id":"a1","title":"Album","file_count":1,
	  "files":[{"success":true,"id":"f1","name":"one.bin","size":10,
	  "hash_sha256":"912046879050495c53a221afdf09a91a0f364bf47daea56479d538c18c4b5151"}]}`
	p, _ := newTestResolver(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(withHash))
	})
	var got Item
	if _, err := p.Resolve(context.Background(), "https://pixeldrain.com/l/a1",
		func(it Item) error { got = it; return nil }); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.SHA256 != "912046879050495c53a221afdf09a91a0f364bf47daea56479d538c18c4b5151" {
		t.Fatalf("album item SHA256 was not carried: %q", got.SHA256)
	}
}

// Finding 4: if file_count and files[] diverge, the list arrived incomplete.
// Silently downloading fewer files and exiting 0 is unacceptable.
func TestAlbumFileCountMismatchIsReported(t *testing.T) {
	const short = `{"success":true,"id":"a1","title":"Album","file_count":5,
	  "files":[{"success":true,"id":"f1","name":"one.bin","size":10}]}`
	p, _ := newTestResolver(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(short))
	})
	var n int
	itemErrs, err := p.Resolve(context.Background(), "https://pixeldrain.com/l/a1",
		func(Item) error { n++; return nil })
	if err != nil {
		t.Fatalf("the album must not fail: %v", err)
	}
	if n != 1 {
		t.Fatalf("%d items, want 1", n)
	}
	if len(itemErrs) == 0 {
		t.Fatal("the file_count mismatch was not reported; the exit code would silently be 0")
	}
	if l, ok := LayerOf(itemErrs[0].Err); !ok || l != LayerParse {
		t.Errorf("Layer = %v, want %v", l, LayerParse)
	}
}

// Finding 11: referer_policy must reach the download request.
// On pixeldrain the policy is "none" so it is empty; but the mechanism must be
// in the right place because bunkr makes the item page Referer mandatory on
// the transfer.
func TestItemHeadersFollowRefererPolicy(t *testing.T) {
	const one = `{"success":true,"id":"f1","name":"one.bin","size":10}`
	run := func(policy string) Item {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(one))
		}))
		t.Cleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		cfg := testCfg()
		cfg.RefererPolicy = policy
		cfg.HTTPClient = &http.Client{Transport: &rewriteTransport{target: u}}
		p := &pixeldrain{cfg: cfg}
		var got Item
		if _, err := p.Resolve(context.Background(), "https://pixeldrain.com/u/f1",
			func(it Item) error { got = it; return nil }); err != nil {
			t.Fatalf("Resolve(%s): %v", policy, err)
		}
		return got
	}

	if h := run(RefererNone); len(h.Headers) != 0 {
		t.Errorf("no Referer must be sent under the none policy: %v", h.Headers)
	}
	if h := run(RefererItemPage); h.Headers["Referer"] != "https://pixeldrain.com/u/f1" {
		t.Errorf("item_page Referer = %q, expected the item page", h.Headers["Referer"])
	}
	if h := run(RefererOrigin); h.Headers["Referer"] != "https://pixeldrain.com/" {
		t.Errorf("origin Referer = %q", h.Headers["Referer"])
	}
}

// Finding 5: a 403 is not always "signed URL expired". pixeldrain also
// reports rate limit and captcha conditions with 403; the classifier must
// separate them, otherwise the tool re-resolves and retries while rate limited.
func TestClassifyStatusSeparatesRateLimitFromExpiredURL(t *testing.T) {
	p := &pixeldrain{cfg: testCfg()}

	resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	body := []byte(`{"success":false,"value":"transfer_limit_exceeded","message":"limit"}`)
	err := p.ClassifyStatus(resp, body)
	if err == nil {
		t.Fatal("a recognizable envelope returned nil; the dl default applies and the 403 counts as expired")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Value != "transfer_limit_exceeded" {
		t.Fatalf("expected APIError: %v", err)
	}
	if l, ok := LayerOf(err); !ok || l != LayerFetch {
		t.Errorf("Layer = %v", l)
	}

	// A Cloudflare challenge must go to its own layer.
	cf := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	cf.Header.Set("CF-Mitigated", "challenge")
	cerr := p.ClassifyStatus(cf, []byte(`{"success":false,"value":"blocked","message":"x"}`))
	if l, ok := LayerOf(cerr); !ok || l != LayerChallenge {
		t.Errorf("challenge Layer = %v, want %v", l, LayerChallenge)
	}

	// An unrecognized body must return nil so dl follows the "signed URL expired" path.
	opaque := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	if err := p.ClassifyStatus(opaque, []byte("<html>Access Denied</html>")); err != nil {
		t.Errorf("an unrecognized body must return nil, got %v", err)
	}
}

// Does the resolver really satisfy the site.StatusClassifier interface?
// If not, the type assertion in the worker silently skips it and finding 5 returns.
func TestPixeldrainImplementsStatusClassifier(t *testing.T) {
	var r Resolver = NewPixeldrain(testCfg())
	if _, ok := r.(StatusClassifier); !ok {
		t.Fatal("pixeldrain does not satisfy the StatusClassifier interface")
	}
}

// --- Paid account API key ---

// pixeldrain's docs: hotlinking is only allowed with a paid account and the
// limit is a PER-FILE counter (downloads > 3 x views). The designed way out
// is the API key: HTTP Basic, empty username, key as password. It must go to
// both the API call and the TRANSFER request; the limit applies on the
// actual transfer.
func TestPixeldrainAPIKeyIsSentAsBasicAuth(t *testing.T) {
	var gotAuth string
	p, _ := newTestResolver(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"id":"xyz789","name":"single.bin","size":42}`))
	})
	p.apiKey = "secret-key"

	var it Item
	if _, err := p.Resolve(context.Background(), "https://pixeldrain.com/u/xyz789",
		func(i Item) error { it = i; return nil }); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// The documented form: Basic base64(":" + api_key)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(":secret-key"))
	if gotAuth != want {
		t.Errorf("Authorization on the API call = %q, want %q", gotAuth, want)
	}
	if it.Headers["Authorization"] != want {
		t.Errorf("Authorization on the transfer header = %q; that is where the limit applies", it.Headers["Authorization"])
	}
}

// Without a key no Authorization header may be sent: sending an empty Basic
// could turn an anonymous request into "authentication failed".
func TestPixeldrainNoAPIKeyMeansNoAuthHeader(t *testing.T) {
	var gotAuth string
	p, _ := newTestResolver(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"id":"xyz789","name":"single.bin","size":42}`))
	})
	var it Item
	_, _ = p.Resolve(context.Background(), "https://pixeldrain.com/u/xyz789",
		func(i Item) error { it = i; return nil })
	if gotAuth != "" {
		t.Errorf("Authorization sent without a key: %q", gotAuth)
	}
	if _, ok := it.Headers["Authorization"]; ok {
		t.Error("Authorization present on the transfer header without a key")
	}
}

// The key must be read from config (Extra); whitespace must be trimmed.
func TestPixeldrainAPIKeyFromConfig(t *testing.T) {
	cfg := testCfg()
	cfg.Extra = map[string]string{ExtraPixeldrainAPIKey: "  abc123  "}
	p := NewPixeldrain(cfg).(*pixeldrain)
	if p.apiKey != "abc123" {
		t.Errorf("apiKey = %q", p.apiKey)
	}
}
