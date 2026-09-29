package site

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func bunkrCfg() SiteConfig {
	return SiteConfig{
		Name:          BunkrName,
		Domains:       []string{"bunkr.ws", "bunkr.ac", "bunkr.black"},
		LegacyDomains: []string{"bunkr.la", "bunkr.su"},
		MatchPatterns: []string{"bunkr.*", "bunkrr.*"},
		RefererPolicy: RefererNone,
		CanaryURLs:    []string{"https://bunkr.ws/"},
		Extra: map[string]string{
			ExtraAPIEndpoint:  "https://api.test/api/v",
			ExtraDLOrigin:     "https://get.test",
			ExtraXORPrefix:    "SECRET_KEY_",
			ExtraSignEndpoint: "https://sign.test/sign",
		},
	}.WithDefaults()
}

// hostRouter is a RoundTripper that produces responses by host name.
// It makes it possible to test domain rotation fully offline: we set up by
// hand which domain returns 403 and which one gives a TLS error.
type hostRouter struct {
	handlers map[string]http.HandlerFunc
	failures map[string]error
	seen     []string
}

func (h *hostRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	h.seen = append(h.seen, host+req.URL.Path)
	if err := h.failures[host]; err != nil {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: err}
	}
	fn, ok := h.handlers[host]
	if !ok {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("unknown host")}
	}
	rec := httptest.NewRecorder()
	fn(rec, req)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

func newBunkr(t *testing.T, rt *hostRouter) *bunkr {
	t.Helper()
	cfg := bunkrCfg()
	cfg.HTTPClient = &http.Client{Transport: rt}
	return NewBunkr(cfg).(*bunkr)
}

func forbidden(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("CF-Mitigated", "challenge")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte("<title>Just a moment...</title>"))
}

// --- URL recognition ---

func TestBunkrMatch(t *testing.T) {
	b := NewBunkr(bunkrCfg())
	ok := []string{
		"https://bunkr.ws/a/ABC123",
		"https://bunkr.ac/a/ABC123",
		"https://app.bunkr.ws/a/ABC123",
		"https://bunkr.ws/f/slug-here",
		"https://bunkr.ws/v/slug-here",
		"https://bunkr.ws/i/slug-here",
		"https://bunkr.ws/d/slug-here",
		// LegacyDomains are RECOGNIZED (not used for fetching, separate test).
		"https://bunkr.la/a/ABC123",
		// Wildcard: a new TLD missing from the list must be recognized.
		"https://bunkr.xyz/a/ABC123",
		"https://bunkrr.pk/a/ABC123",
		"bunkr.ws/a/ABC123",
	}
	for _, u := range ok {
		if !b.Match(u) {
			t.Errorf("Match(%q) = false, want true", u)
		}
	}
	bad := []string{
		"https://pixeldrain.com/l/abc",
		// SECURITY: a wildcard matches a single label; a hostile subdomain doesn't match.
		"https://bunkr.attacker.com/a/ABC",
		"https://bunkr.ws/",
		"https://bunkr.ws/a/",
		"https://bunkr.ws/x/slug",
		"",
	}
	for _, u := range bad {
		if b.Match(u) {
			t.Errorf("Match(%q) = true, want false", u)
		}
	}
}

// --- XOR decryption ---

// Key derivation is the only non-fixed part of the decryption: the key is
// derived from timestamp/3600, i.e. tied to an hourly window.
//
// This test RELIES ON LIVE VERIFICATION but does NOT EMBED live data. On
// 2026-09-09, on a real API response, both this implementation and an
// independent Python implementation produced the same URL (timestamp
// 1788991464 -> SECRET_KEY_496942, the decrypted URL had the form
// "https://<host>.cdn.cr/<uuid>.mp4"). The real base64 and UUID aren't
// written to the repo: the first changes on every call, the second points to
// someone else's content. What is pinned here is the FORMULA.
func TestXORKeyDerivation(t *testing.T) {
	cases := []struct {
		timestamp int64
		want      string
	}{
		{1788991464, "SECRET_KEY_496942"},
		{0, "SECRET_KEY_0"},
		{3599, "SECRET_KEY_0"},
		{3600, "SECRET_KEY_1"},
	}
	for _, c := range cases {
		got := "SECRET_KEY_" + fmt.Sprint(c.timestamp/3600)
		if got != c.want {
			t.Errorf("timestamp %d -> %q, want %q", c.timestamp, got, c.want)
		}
	}

	// That the formula produces the real URL is verified with the real key
	// over a synthetic URL.
	const timestamp = 1788991464
	key := []byte("SECRET_KEY_" + fmt.Sprint(timestamp/3600))
	plain := "https://c3bc-b.cdn.cr/419b02f6-8abe-46aa-bc4a-9fc5b24095cd.mp4"
	enc := make([]byte, len(plain))
	for i := 0; i < len(plain); i++ {
		enc[i] = plain[i] ^ key[i%len(key)]
	}
	got, err := decryptXOR(base64.StdEncoding.EncodeToString(enc), key)
	if err != nil {
		t.Fatalf("decryptXOR: %v", err)
	}
	if got != plain {
		t.Fatalf("got %q, want %q", got, plain)
	}
}

func TestDecryptXORRoundTrip(t *testing.T) {
	plain := "https://cdn.example/abc-def.mp4"
	key := []byte("SECRET_KEY_123456")
	raw := []byte(plain)
	enc := make([]byte, len(raw))
	for i := range raw {
		enc[i] = raw[i] ^ key[i%len(key)]
	}
	got, err := decryptXOR(base64.StdEncoding.EncodeToString(enc), key)
	if err != nil {
		t.Fatal(err)
	}
	if got != plain {
		t.Fatalf("got %q, want %q", got, plain)
	}
}

func TestDecryptXORErrors(t *testing.T) {
	if _, err := decryptXOR("Zm9v", nil); err == nil {
		t.Error("an empty key must fail")
	}
	if _, err := decryptXOR("this is not base64!!!", []byte("k")); err == nil {
		t.Error("invalid base64 must fail")
	}
}

// --- Album parsing ---

// Field names and order were verified on 2026-09-09 against a live bunkr.ws
// album page: id, name, original, slug, type, extension, size, timestamp,
// thumbnail, cdnEndpoint.
const albumFixture = `<html><head>
<meta property="og:title" content="Holiday Album 2026" />
</head><body>
<span class="font-semibold">(3 files)</span>
<script>
window.albumFiles = [
{
  id: 51537490,
  name: "419b02f6-8abe-46aa-bc4a-9fc5b24095cd.mp4",
  original: "First Video - Zoë & Chloé.mp4",
  slug: "first-video-abc",
  type: "video/mp4",
  extension: ".mp4",
  size:  1946234880 ,
  timestamp: "12:34:56 20/07/2026",
  thumbnail: "https://i.bunkr.ru/thumbs/first.png",
  cdnEndpoint: "https://c3bc-b.cdn.cr"
},
{
  id: 51537491,
  name: "aaa.mp4",
  original: "Second { curly } braces.mp4",
  slug: "second-video-def",
  type: "video/mp4",
  extension: ".mp4",
  size:  100 ,
  timestamp: "01:02:03 21/07/2026",
  thumbnail: "https://i.bunkr.ru/thumbs/second.png",
  cdnEndpoint: "https://c3su-b.cdn.cr"
},
{
  id: 51537492,
  name: "bbb.png",
  slug: "third-image-ghi",
  type: "image/png",
  extension: ".png",
  size:  broken ,
  timestamp: "",
  thumbnail: "",
  cdnEndpoint: ""
}
];
</script></body></html>`

func TestParseAlbumFiles(t *testing.T) {
	files, err := parseAlbumFiles(albumFixture)
	if err != nil {
		t.Fatalf("parseAlbumFiles: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("%d items, want 3", len(files))
	}

	if files[0].ID != "51537490" {
		t.Errorf("id = %q", files[0].ID)
	}
	if files[0].Name != "First Video - Zoë & Chloé.mp4" {
		t.Errorf("original name = %q", files[0].Name)
	}
	if files[0].Slug != "first-video-abc" {
		t.Errorf("slug = %q", files[0].Slug)
	}
	if files[0].Size != 1946234880 {
		t.Errorf("size = %d (the source has a double space and a space before the comma)", files[0].Size)
	}

	// Curly braces inside a value: block splitting must not break.
	if files[1].Name != "Second { curly } braces.mp4" {
		t.Errorf("name with curly braces broke: %q", files[1].Name)
	}

	// Broken size -> -1, the item isn't skipped.
	if files[2].Size != -1 {
		t.Errorf("broken size = %d, want -1", files[2].Size)
	}
	// No original -> slug + extension fallback.
	if files[2].Name != "third-image-ghi.png" {
		t.Errorf("fallback name = %q", files[2].Name)
	}
}

func TestParseAlbumFilesMissingMarker(t *testing.T) {
	if _, err := parseAlbumFiles("<html>empty page</html>"); err == nil {
		t.Fatal("expected an error without window.albumFiles")
	}
}

// Blocks are split by curly brace BALANCE, not by a line-ending pattern
// ("\n},\n"): if formatting changes, that pattern silently produces one giant
// item and the album collapses into a single file.
func TestSplitJSObjectsHandlesBracesInStrings(t *testing.T) {
	body := `{ a: "i{n}side", b: 1 }, { c: 'x}y', d: 2 }`
	got := splitJSObjects(body)
	if len(got) != 2 {
		t.Fatalf("%d blocks, want 2: %q", len(got), got)
	}
}

func TestSplitJSObjectsSingleLine(t *testing.T) {
	// Output squeezed onto a single line must work too.
	body := `{id: 1, original: "a.mp4"},{id: 2, original: "b.mp4"}`
	if got := splitJSObjects(body); len(got) != 2 {
		t.Fatalf("%d blocks, want 2", len(got))
	}
}

// --- Domain rotation ---

func albumHandler(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.URL.RawQuery, "advanced=1") {
		// Without advanced=1 no albumFiles arrive; we pin that here.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>no advanced</html>"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(albumFixture))
}

func TestBunkrRotatesPastChallengedDomain(t *testing.T) {
	rt := &hostRouter{
		handlers: map[string]http.HandlerFunc{
			"bunkr.ws":    forbidden,    // Cloudflare challenge
			"bunkr.ac":    albumHandler, // working
			"bunkr.black": forbidden,
		},
	}
	b := newBunkr(t, rt)
	body, root, err := b.fetchWithRotation(context.Background(), "/a/X?advanced=1", "")
	if err != nil {
		t.Fatalf("rotation failed: %v", err)
	}
	if root != "https://bunkr.ac" {
		t.Errorf("root = %q, want bunkr.ac", root)
	}
	if !strings.Contains(string(body), "window.albumFiles") {
		t.Error("the album page did not arrive")
	}
	burned := b.Burned()
	if why, ok := burned["bunkr.ws"]; !ok || !strings.Contains(why, "Cloudflare") {
		t.Errorf("bunkr.ws not eliminated or wrong reason: %q", why)
	}
}

// THE REAL FINDING: gallery-dl eliminates domains only on 403. A measurement
// showed that from a network in Turkey the block arrives as a certificate
// error and a connection timeout. Rotation must cover those too, otherwise it
// gets stuck on a blocked domain.
func TestBunkrRotatesPastConnectionFailure(t *testing.T) {
	rt := &hostRouter{
		handlers: map[string]http.HandlerFunc{"bunkr.black": albumHandler},
		failures: map[string]error{
			"bunkr.ws": errors.New("connection refused"),
			"bunkr.ac": errors.New("i/o timeout"),
		},
	}
	b := newBunkr(t, rt)
	_, root, err := b.fetchWithRotation(context.Background(), "/a/X?advanced=1", "")
	if err != nil {
		t.Fatalf("no rotation on a connection error: %v", err)
	}
	if root != "https://bunkr.black" {
		t.Errorf("root = %q", root)
	}
	burned := b.Burned()
	if len(burned) != 2 {
		t.Fatalf("%d domains eliminated, want 2: %v", len(burned), burned)
	}
	for _, d := range []string{"bunkr.ws", "bunkr.ac"} {
		if !strings.Contains(burned[d], "could not connect") {
			t.Errorf("%s reason = %q", d, burned[d])
		}
	}
}

func TestBunkrAllDomainsBurned(t *testing.T) {
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"bunkr.ws": forbidden, "bunkr.ac": forbidden, "bunkr.black": forbidden,
	}}
	b := newBunkr(t, rt)
	_, _, err := b.fetchWithRotation(context.Background(), "/a/X", "")
	if !errors.Is(err, ErrAllDomainsBurned) {
		t.Fatalf("expected ErrAllDomainsBurned: %v", err)
	}
	// The reasons must be recorded for diagnosis.
	if l, ok := LayerOf(err); !ok || l != LayerChallenge {
		t.Errorf("Layer = %v, want %v", l, LayerChallenge)
	}
}

// A permanent error must NOT TRIGGER rotation: a 404 says the domain is fine
// but the content isn't there. Trying every domain means 14 empty requests.
func TestBunkrPermanentErrorDoesNotRotate(t *testing.T) {
	notFound := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"bunkr.ws": notFound, "bunkr.ac": albumHandler, "bunkr.black": albumHandler,
	}}
	b := newBunkr(t, rt)
	if _, _, err := b.fetchWithRotation(context.Background(), "/a/X", ""); err == nil {
		t.Fatal("a 404 should have failed")
	}
	if len(b.Burned()) != 0 {
		t.Errorf("a domain was eliminated on 404: %v", b.Burned())
	}
	if len(rt.seen) != 1 {
		t.Errorf("%d requests sent, want 1: %v", len(rt.seen), rt.seen)
	}
}

// fakeClock tests elimination durations without waiting in real time.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func withClock(b *bunkr) *fakeClock {
	c := &fakeClock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	b.now = c.now
	return c
}

// A local network outage (DNS dropping for a few seconds while the VPN
// switches) eliminates every domain. If elimination is PERMANENT, bunkr stayed
// dead until the app was restarted. A network-caused elimination must expire
// soon, and the domains must return in config ORDER (order: the working
// domain first).
func TestBunkrNetworkBurnExpires(t *testing.T) {
	down := &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}
	rt := &hostRouter{failures: map[string]error{
		"bunkr.ws": down, "bunkr.ac": down, "bunkr.black": down,
	}}
	b := newBunkr(t, rt)
	clock := withClock(b)

	if _, _, err := b.fetchWithRotation(context.Background(), "/a/X?advanced=1", ""); !errors.Is(err, ErrAllDomainsBurned) {
		t.Fatalf("expected ErrAllDomainsBurned without a network: %v", err)
	}

	// The network is back but the duration hasn't passed: still eliminated.
	rt.failures = nil
	rt.handlers = map[string]http.HandlerFunc{"bunkr.ws": albumHandler, "bunkr.ac": albumHandler, "bunkr.black": albumHandler}
	clock.advance(30 * time.Second)
	if got := b.roots(); len(got) != 0 {
		t.Fatalf("%v came back after 30 s; a network elimination should last 2 min", got)
	}

	clock.advance(2 * time.Minute)
	_, root, err := b.fetchWithRotation(context.Background(), "/a/X?advanced=1", "")
	if err != nil {
		t.Fatalf("bunkr still dead after the duration passed: %v", err)
	}
	if root != "https://bunkr.ws" {
		t.Errorf("root = %q; domains should have come back in config order", root)
	}
	if len(b.Burned()) != 0 {
		t.Errorf("expired eliminations were not cleared: %v", b.Burned())
	}
}

// A Cloudflare challenge and a certificate error are not network outages but
// a real BLOCK: they must last longer than a network elimination, otherwise
// the blocked domain is retried for nothing every two minutes.
func TestBunkrChallengeBurnOutlastsNetworkBurn(t *testing.T) {
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"bunkr.ws": forbidden, "bunkr.ac": albumHandler, "bunkr.black": albumHandler,
	}}
	b := newBunkr(t, rt)
	clock := withClock(b)
	if _, _, err := b.fetchWithRotation(context.Background(), "/a/X?advanced=1", ""); err != nil {
		t.Fatal(err)
	}
	clock.advance(5 * time.Minute)
	for _, d := range b.roots() {
		if d == "bunkr.ws" {
			t.Fatal("the challenge elimination expired within 5 min")
		}
	}
	clock.advance(30 * time.Minute)
	if got := b.roots(); len(got) != 3 || got[0] != "bunkr.ws" {
		t.Errorf("pool after 30 min is %v; bunkr.ws should be back at the front", got)
	}
}

// Switching to the fallback endpoint must not be permanent: the primary may
// have been temporarily unreachable (VPN, network). After a while the primary
// is tried again.
func TestBunkrPrimaryRetriedAfterCooldown(t *testing.T) {
	rt := &hostRouter{
		failures: map[string]error{"api.test": errors.New("timeout")},
		handlers: map[string]http.HandlerFunc{
			"oldapi.test": func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(legacyBody("https://c9.test/a.m4v")))
			},
		},
	}
	b := newBunkrFallback(t, rt)
	clock := withClock(b)
	if _, _, err := b.resolveFileURL(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}

	// The primary recovered.
	rt.failures = nil
	rt.handlers["api.test"] = apiHandler(t, "https://get.test/file/")
	clock.advance(2 * time.Minute)
	if got, _, _ := b.resolveFileURL(context.Background(), "1"); !strings.Contains(got, "c9.test") {
		t.Fatalf("went back to the primary before the cooldown passed: %q", got)
	}
	clock.advance(10 * time.Minute)
	got, _, err := b.resolveFileURL(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://cdn.test/file.mp4" {
		t.Errorf("URL after the cooldown = %q; the primary should have been retried", got)
	}
}

// A wildcard entry must NOT ENTER the rotation pool: no request can go to "bunkr.*".
func TestBunkrWildcardNotInRotationPool(t *testing.T) {
	cfg := bunkrCfg()
	cfg.Domains = append(cfg.Domains, "bunkr.*")
	b := NewBunkr(cfg).(*bunkr)
	for _, d := range b.roots() {
		if strings.ContainsRune(d, '*') {
			t.Fatalf("wildcard in the rotation pool: %q", d)
		}
	}
}

// --- End-to-end resolution ---

func apiHandler(t *testing.T, wantReferer string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("API method = %s, want POST", r.Method)
		}
		// Referer and Origin are MANDATORY: the endpoint checks them.
		if got := r.Header.Get("Referer"); !strings.HasPrefix(got, wantReferer) {
			t.Errorf("API Referer = %q, should start with %q", got, wantReferer)
		}
		if got := r.Header.Get("Origin"); got != "https://get.test" {
			t.Errorf("API Origin = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://cdn.test/file.mp4","encrypted":false,"timestamp":0}`))
	}
}

// signHandler imitates the signing service. Like the real service it only
// looks at the path and produces a token.
func signHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"token":"tok-` + strings.TrimPrefix(path, "/") + `","ex":1789054914}`))
}

func TestBunkrResolveAlbum(t *testing.T) {
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"bunkr.ws":  albumHandler,
		"api.test":  apiHandler(t, "https://get.test/file/"),
		"sign.test": signHandler,
	}}
	b := newBunkr(t, rt)

	var items []Item
	itemErrs, err := b.Resolve(context.Background(), "https://bunkr.ws/a/ABC123",
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

	if items[0].Dir != "Holiday Album 2026" {
		t.Errorf("Dir = %q", items[0].Dir)
	}
	if items[0].Filename != "First Video - Zoë & Chloé.mp4" {
		t.Errorf("Filename = %q", items[0].Filename)
	}
	// The URL now comes back RAW: the signature is taken with PrepareURL when the download starts.
	if items[0].URL != "https://cdn.test/file.mp4" {
		t.Errorf("URL = %q", items[0].URL)
	}
	// SourcePage must be built from the slug: ResolveOne depends on it.
	if items[0].SourcePage != "https://bunkr.ws/f/first-video-abc" {
		t.Errorf("SourcePage = %q", items[0].SourcePage)
	}
	// The download Referer comes from the PROTOCOL, not the POLICY; it must be
	// set even though referer_policy is "none".
	if ref := items[0].Headers["Referer"]; ref != "https://get.test/file/51537490" {
		t.Errorf("download Referer = %q", ref)
	}
	if items[0].Size != 1946234880 {
		t.Errorf("Size = %d", items[0].Size)
	}
	// bunkr doesn't give sha256.
	if items[0].SHA256 != "" {
		t.Errorf("SHA256 came back set: %q", items[0].SHA256)
	}
}

// LAZY SIGNING: the signing service must never be called during resolution.
// Otherwise files that are skipped or only listed get signed for nothing, and
// the token of the file at the end of the queue dies before its turn.
func TestBunkrResolveDoesNotSign(t *testing.T) {
	signCalls := 0
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"bunkr.ws": albumHandler,
		"api.test": apiHandler(t, "https://get.test/file/"),
		"sign.test": func(w http.ResponseWriter, r *http.Request) {
			signCalls++
			signHandler(w, r)
		},
	}}
	b := newBunkr(t, rt)
	if _, err := b.Resolve(context.Background(), "https://bunkr.ws/a/ABC123",
		func(Item) error { return nil }); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if signCalls != 0 {
		t.Fatalf("%d signing requests went out during resolution, want 0", signCalls)
	}
}

// If a single item's API call fails, the album must NOT fail; ItemErrors must be collected.
func TestBunkrResolveCollectsPerItemErrors(t *testing.T) {
	calls := 0
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"bunkr.ws":  albumHandler,
		"sign.test": signHandler,
		"api.test": func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 2 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"url":"https://cdn.test/x.mp4","encrypted":false}`))
		},
	}}
	b := newBunkr(t, rt)
	var n int
	itemErrs, err := b.Resolve(context.Background(), "https://bunkr.ws/a/ABC",
		func(Item) error { n++; return nil })
	if err != nil {
		t.Fatalf("the album should not have failed: %v", err)
	}
	if n != 2 {
		t.Errorf("%d items yielded, want 2", n)
	}
	if len(itemErrs) != 1 {
		t.Fatalf("%d item errors, want 1", len(itemErrs))
	}
	if l, ok := LayerOf(itemErrs[0].Err); !ok || l != LayerItemPage {
		t.Errorf("Layer = %v, want %v", l, LayerItemPage)
	}
}

func TestBunkrResolveMediaAndResolveOne(t *testing.T) {
	mediaPage := `<html><head><meta property="og:title" content="Single File.mp4" /></head>
<body><div data-file-id="99887766"></div></body></html>`
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"bunkr.ws": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(mediaPage))
		},
		"api.test":  apiHandler(t, "https://get.test/file/99887766"),
		"sign.test": signHandler,
	}}
	b := newBunkr(t, rt)

	got, err := b.ResolveOne(context.Background(), "https://bunkr.ws/f/single-file")
	if err != nil {
		t.Fatalf("ResolveOne: %v", err)
	}
	if got.Filename != "Single File.mp4" {
		t.Errorf("Filename = %q", got.Filename)
	}
	if got.Headers["Referer"] != "https://get.test/file/99887766" {
		t.Errorf("Referer = %q", got.Headers["Referer"])
	}
}

// og:title is an HTML attribute: characters like "&" arrive as entities.
// Without decoding, "&amp;" showed up in folder and file names.
func TestBunkrTitleEntitiesAreDecoded(t *testing.T) {
	mediaPage := `<html><head><meta property="og:title" content="Tom &amp; Jerry&#39;s.mp4" /></head>
<body><div data-file-id="99887766"></div></body></html>`
	album := strings.Replace(albumFixture, "Holiday Album 2026", "Tom &amp; Jerry&#39;s", 1)
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"bunkr.ws": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/a/") {
				_, _ = w.Write([]byte(album))
				return
			}
			_, _ = w.Write([]byte(mediaPage))
		},
		"api.test":  apiHandler(t, "https://get.test/file/"),
		"sign.test": signHandler,
	}}
	b := newBunkr(t, rt)

	got, err := b.ResolveOne(context.Background(), "https://bunkr.ws/f/single-file")
	if err != nil {
		t.Fatalf("ResolveOne: %v", err)
	}
	if got.Filename != "Tom & Jerry's.mp4" {
		t.Errorf("Filename = %q", got.Filename)
	}

	var items []Item
	if _, err := b.Resolve(context.Background(), "https://bunkr.ws/a/ABC123",
		func(it Item) error { items = append(items, it); return nil }); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(items) == 0 || items[0].Dir != "Tom & Jerry's" {
		t.Errorf("Dir = %q", items[0].Dir)
	}
}

func TestBunkrResolveOneRejectsAlbum(t *testing.T) {
	b := NewBunkr(bunkrCfg())
	if _, err := b.ResolveOne(context.Background(), "https://bunkr.ws/a/ABC"); err == nil {
		t.Fatal("expected an error for an album URL")
	}
}

// --- Maintenance placeholder ---

// For deleted/maintenance files bunkr returns a placeholder video with 200,
// NOT 404. The status is clean, the content is garbage. Without this check
// the tool counts the garbage as "downloaded successfully".
func TestBunkrValidateResponseCatchesMaintenance(t *testing.T) {
	b := NewBunkr(bunkrCfg()).(*bunkr)

	for _, name := range []string{"/maint.mp4", "/maintenance-vid.mp4"} {
		req, _ := http.NewRequest(http.MethodGet, "https://cdn.test"+name, nil)
		resp := &http.Response{StatusCode: 200, Request: req, Header: http.Header{}}
		err := b.ValidateResponse(resp)
		if err == nil {
			t.Fatalf("the %s maintenance placeholder was not caught", name)
		}
		if l, ok := LayerOf(err); !ok || l != LayerCDN {
			t.Errorf("Layer = %v, want %v", l, LayerCDN)
		}
	}

	req, _ := http.NewRequest(http.MethodGet, "https://cdn.test/real-file.mp4", nil)
	resp := &http.Response{StatusCode: 200, Request: req, Header: http.Header{}}
	if err := b.ValidateResponse(resp); err != nil {
		t.Errorf("a real file was rejected: %v", err)
	}
}

func TestBunkrClassifyStatus(t *testing.T) {
	b := NewBunkr(bunkrCfg()).(*bunkr)

	// A challenge must go to its own layer.
	cf := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	cf.Header.Set("CF-Mitigated", "challenge")
	err := b.ClassifyStatus(cf, nil)
	if l, ok := LayerOf(err); !ok || l != LayerChallenge {
		t.Errorf("challenge Layer = %v", l)
	}

	// A 403 that isn't a challenge: the signed URL may have expired; let the
	// downloader re-resolve with ResolveOne -> must return nil.
	plain := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	if err := b.ClassifyStatus(plain, []byte("<h1>403 Forbidden</h1>")); err != nil {
		t.Errorf("a plain 403 should have returned nil: %v", err)
	}

	// Anything other than 403 must be left alone.
	other := &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}}
	if err := b.ClassifyStatus(other, nil); err != nil {
		t.Errorf("a 500 should have returned nil: %v", err)
	}
}

// --- Redirects ---

// Automatic redirect following MUST be off: a Cloudflare challenge can also
// arrive as a redirect and automatic following swallows that signal.
func TestBunkrFollowsRedirectsManually(t *testing.T) {
	hops := 0
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"bunkr.ws": func(w http.ResponseWriter, r *http.Request) {
			hops++
			if r.URL.Path == "/a/X" {
				w.Header().Set("Location", "/a/Y?advanced=1")
				w.WriteHeader(http.StatusFound)
				return
			}
			_, _ = w.Write([]byte(albumFixture))
		},
	}}
	b := newBunkr(t, rt)
	body, _, err := b.fetchWithRotation(context.Background(), "/a/X", "")
	if err != nil {
		t.Fatalf("the redirect could not be followed: %v", err)
	}
	if hops != 2 {
		t.Errorf("%d requests, want 2", hops)
	}
	if !strings.Contains(string(body), "window.albumFiles") {
		t.Error("the target page did not arrive")
	}
}

func TestBunkrRedirectLoopIsBounded(t *testing.T) {
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"bunkr.ws": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/loop")
			w.WriteHeader(http.StatusFound)
		},
	}}
	cfg := bunkrCfg()
	cfg.Domains = []string{"bunkr.ws"}
	cfg.HTTPClient = &http.Client{Transport: rt}
	b := NewBunkr(cfg).(*bunkr)
	if _, _, err := b.fetchWithRotation(context.Background(), "/loop", ""); err == nil {
		t.Fatal("an endless redirect must be stopped")
	}
}

// --- Signed download flow (2026-09-10) ---

// MEASURED: the CDN rejects an unsigned GET WITHOUT LOOKING at the file. For
// an existing file and a made-up name the response is byte-for-byte
// identical (403, same body, same headers). That is why signing is mandatory
// and looking at a 403 to say "the file was deleted" would be wrong.
func TestBunkrSignsCurrentAPIShape(t *testing.T) {
	var signedPath string
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"api.test": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"mediafiles":"https://c1.test","path":"/storage/media/video-abc.mp4","original":"Real Name é.mp4"}`))
		},
		"sign.test": func(w http.ResponseWriter, r *http.Request) {
			signedPath = r.URL.Query().Get("path")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"abc123","ex":1789054914}`))
		},
	}}
	b := newBunkr(t, rt)

	raw, _, err := b.resolveFileURL(context.Background(), "555")
	if err != nil {
		t.Fatalf("resolveFileURL: %v", err)
	}
	got, err := b.PrepareURL(context.Background(), raw)
	if err != nil {
		t.Fatalf("PrepareURL: %v", err)
	}

	// The DECODED form of the path must go to the signing service.
	if signedPath != "/storage/media/video-abc.mp4" {
		t.Errorf("signed path = %q", signedPath)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("could not parse the produced URL: %v", err)
	}
	if u.Host != "c1.test" || u.Path != "/storage/media/video-abc.mp4" {
		t.Errorf("URL built wrong: %q", got)
	}
	q := u.Query()
	if q.Get("token") != "abc123" {
		t.Errorf("token = %q", q.Get("token"))
	}
	if q.Get("ex") != "1789054914" {
		t.Errorf("ex = %q", q.Get("ex"))
	}
	// n is the original name the CDN uses in Content-Disposition.
	if q.Get("n") != "Real Name é.mp4" {
		t.Errorf("n = %q", q.Get("n"))
	}
}

// The old shape (full URL encrypted with XOR) must still work; it must be signed too.
func TestBunkrSignsLegacyAPIShape(t *testing.T) {
	plain := "https://c9.test/old/file.mp4"
	key := []byte("SECRET_KEY_0")
	enc := make([]byte, len(plain))
	for i := 0; i < len(plain); i++ {
		enc[i] = plain[i] ^ key[i%len(key)]
	}
	body := `{"url":"` + base64.StdEncoding.EncodeToString(enc) + `","encrypted":true,"timestamp":0}`

	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"api.test": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		},
		"sign.test": signHandler,
	}}
	b := newBunkr(t, rt)

	raw, _, err := b.resolveFileURL(context.Background(), "42")
	if err != nil {
		t.Fatalf("resolveFileURL: %v", err)
	}
	got, err := b.PrepareURL(context.Background(), raw)
	if err != nil {
		t.Fatalf("PrepareURL: %v", err)
	}
	if !strings.HasPrefix(got, plain+"?") {
		t.Errorf("old-shape URL was corrupted: %q", got)
	}
	if !strings.Contains(got, "token=") {
		t.Errorf("old-shape URL was not signed: %q", got)
	}
}

// If the signing service is down the download URL must NOT BE MADE UP: an
// unsigned URL gets 403 anyway and the error shows up as "403", misleading
// the diagnosis.
func TestBunkrSignFailureIsAnError(t *testing.T) {
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"api.test": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"mediafiles":"https://c1.test","path":"/storage/media/x.mp4"}`))
		},
		"sign.test": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	}}
	b := newBunkr(t, rt)

	raw, _, err := b.resolveFileURL(context.Background(), "7")
	if err != nil {
		t.Fatalf("resolveFileURL: %v", err)
	}
	if _, err := b.PrepareURL(context.Background(), raw); err == nil {
		t.Fatal("expected an error when the signing service returns 500")
	} else if l, ok := LayerOf(err); !ok || l != LayerCDN {
		t.Errorf("layer = %v, want %v", l, LayerCDN)
	}
}

func TestBunkrSignRejectsTokenlessResponse(t *testing.T) {
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"api.test": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"mediafiles":"https://c1.test","path":"/storage/media/x.mp4"}`))
		},
		"sign.test": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ex":123}`))
		},
	}}
	b := newBunkr(t, rt)

	raw, _, err := b.resolveFileURL(context.Background(), "7")
	if err != nil {
		t.Fatalf("resolveFileURL: %v", err)
	}
	if _, err := b.PrepareURL(context.Background(), raw); err == nil {
		t.Fatal("a signing response without a token should have failed")
	}
}

// If the API gives a URL in neither the new nor the old shape: a clear error.
func TestBunkrEmptyAPIResponse(t *testing.T) {
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"api.test": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		},
		"sign.test": signHandler,
	}}
	b := newBunkr(t, rt)

	if _, _, err := b.resolveFileURL(context.Background(), "7"); err == nil {
		t.Fatal("expected an error for an empty response")
	}
}

// Even if mediafiles ends with "/", the path must not be doubled.
func TestBunkrRawURLJoinsCleanly(t *testing.T) {
	b := NewBunkr(bunkrCfg()).(*bunkr)
	got, err := b.rawFileURL(bunkrAPIResponse{
		MediaFiles: "https://c1.test/",
		Path:       "/storage/media/a.mp4",
	}, "1")
	if err != nil {
		t.Fatalf("rawFileURL: %v", err)
	}
	if got != "https://c1.test/storage/media/a.mp4" {
		t.Errorf("URL = %q", got)
	}
}

// MEASURED: if the file name contains '#', url.Parse takes it for a fragment
// and CUTS the path. Because the path is assigned as a FIELD instead of being
// concatenated and parsed, the name is preserved. If this broke, the signature
// would be taken for the wrong path and the CDN would return 403 — the
// hardest error class to diagnose, looking like "the signature expired".
func TestBunkrRawURLKeepsSpecialCharsInPath(t *testing.T) {
	b := NewBunkr(bunkrCfg()).(*bunkr)
	for _, name := range []string{"/storage/media/track #3.mp4", "/storage/media/a?b.mp4"} {
		got, err := b.rawFileURL(bunkrAPIResponse{MediaFiles: "https://c1.test", Path: name}, "1")
		if err != nil {
			t.Fatalf("rawFileURL(%q): %v", name, err)
		}
		u, perr := url.Parse(got)
		if perr != nil {
			t.Fatalf("could not parse the produced URL: %v", perr)
		}
		if u.Fragment != "" {
			t.Errorf("%q: the tail of the file name escaped into the fragment (%q): %q", name, u.Fragment, got)
		}
		if u.Path != name {
			t.Errorf("%q: the path to be signed was corrupted: %q", name, u.Path)
		}
	}
}

// If the path doesn't start with "/", one must be added; otherwise a value
// like "@evil.tld/x" could change the host during concatenation.
func TestBunkrRawURLForcesLeadingSlash(t *testing.T) {
	b := NewBunkr(bunkrCfg()).(*bunkr)
	got, err := b.rawFileURL(bunkrAPIResponse{MediaFiles: "https://c1.test", Path: "@evil.tld/x.mp4"}, "1")
	if err != nil {
		t.Fatalf("rawFileURL: %v", err)
	}
	u, _ := url.Parse(got)
	if u.Host != "c1.test" {
		t.Fatalf("host changed: %q (%q)", u.Host, got)
	}
}

// If the API gives a non-https base, the download must not silently drop to plaintext.
func TestBunkrRawURLRejectsNonHTTPSBase(t *testing.T) {
	b := NewBunkr(bunkrCfg()).(*bunkr)
	for _, base := range []string{"http://c1.test", "ftp://c1.test", "/no-scheme"} {
		if got, err := b.rawFileURL(bunkrAPIResponse{MediaFiles: base, Path: "/x.mp4"}, "1"); err == nil {
			t.Errorf("base %q accepted: %q", base, got)
		}
	}
}

// If ex were missing, 0 would be written; the CDN returns a plain 403 and
// ClassifyStatus would count it as "signature expired". So the user would see
// the wrong error.
func TestBunkrSignRejectsMissingExpiry(t *testing.T) {
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"api.test": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"mediafiles":"https://c1.test","path":"/storage/media/x.mp4"}`))
		},
		"sign.test": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"token":"abc123"}`))
		},
	}}
	b := newBunkr(t, rt)
	raw, _, err := b.resolveFileURL(context.Background(), "7")
	if err != nil {
		t.Fatalf("resolveFileURL: %v", err)
	}
	if got, err := b.PrepareURL(context.Background(), raw); err == nil {
		t.Fatalf("a signing response without ex was accepted: %q", got)
	}
}

// The signing record written to disk with --record must not contain the
// token VALUE: it is a time-limited credential granting access to the file,
// and recordings are written with 0644.
func TestRedactTokenRemovesTheToken(t *testing.T) {
	out := redactToken([]byte(`{"token":"secret-value-123","ex":1789054914}`))
	if strings.Contains(string(out), "secret-value-123") {
		t.Fatalf("the token was written to the record: %s", out)
	}
	if !strings.Contains(string(out), "1789054914") {
		t.Errorf("ex was lost, the diagnostic value drops: %s", out)
	}
	// An unparseable body (HTML error page) must stay as is.
	raw := []byte("<html>503</html>")
	if string(redactToken(raw)) != string(raw) {
		t.Errorf("the error body was corrupted: %s", redactToken(raw))
	}
}

// --- Fallback API endpoint ---

func fallbackCfg() SiteConfig {
	cfg := bunkrCfg()
	cfg.Extra[ExtraFallbackAPI] = "https://oldapi.test/api/v"
	cfg.Extra[ExtraLegacyPrefix] = "/storage/media"
	return cfg
}

func newBunkrFallback(t *testing.T, rt *hostRouter) *bunkr {
	t.Helper()
	cfg := fallbackCfg()
	cfg.HTTPClient = &http.Client{Transport: rt}
	return NewBunkr(cfg).(*bunkr)
}

// legacyBody produces the old endpoint's shape (full URL XORed).
func legacyBody(plain string) string {
	key := []byte("SECRET_KEY_0")
	enc := make([]byte, len(plain))
	for i := 0; i < len(plain); i++ {
		enc[i] = plain[i] ^ key[i%len(key)]
	}
	return `{"url":"` + base64.StdEncoding.EncodeToString(enc) + `","encrypted":true,"timestamp":0}`
}

// MEASURED: on this network dl.bunkr.cr is hijacked at the DNS level. If the
// primary endpoint is UNREACHABLE it must fall back to the fallback endpoint,
// otherwise bunkr only works over a VPN.
func TestBunkrFallsBackWhenPrimaryUnreachable(t *testing.T) {
	rt := &hostRouter{
		failures: map[string]error{"api.test": errors.New("connection timeout")},
		handlers: map[string]http.HandlerFunc{
			"oldapi.test": func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(legacyBody("https://c9.test/file.m4v")))
			},
		},
	}
	b := newBunkrFallback(t, rt)

	got, _, err := b.resolveFileURL(context.Background(), "1")
	if err != nil {
		t.Fatalf("did not fall back to the fallback endpoint: %v", err)
	}
	// The old endpoint doesn't give the storage prefix; it must have been added.
	if got != "https://c9.test/storage/media/file.m4v" {
		t.Fatalf("URL = %q", got)
	}
}

// The decision to fall back must be STICKY: otherwise a 40-file album waits
// for 40 timeouts and the UI freezes for minutes.
func TestBunkrFallbackDecisionIsSticky(t *testing.T) {
	primaryTries := 0
	rt := &hostRouter{
		handlers: map[string]http.HandlerFunc{
			"oldapi.test": func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(legacyBody("https://c9.test/a.m4v")))
			},
		},
	}
	rt.failures = map[string]error{}
	// Count every touch of api.test via hostRouter.seen.
	b := newBunkrFallback(t, rt)
	rt.failures["api.test"] = errors.New("timeout")

	for i := 0; i < 4; i++ {
		if _, _, err := b.resolveFileURL(context.Background(), "1"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	for _, h := range rt.seen {
		if strings.HasPrefix(h, "api.test") {
			primaryTries++
		}
	}
	if primaryTries != 1 {
		t.Fatalf("the primary endpoint was tried %d times, want 1 (the decision is not sticky)", primaryTries)
	}
}

// An answer the server GAVE must not trigger the fallback: "file deleted"
// (400) will be deleted on the fallback endpoint too; retrying only adds delay.
func TestBunkrDoesNotFallBackOnRealAPIError(t *testing.T) {
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"api.test": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
		},
		"oldapi.test": func(w http.ResponseWriter, r *http.Request) {
			t.Error("the fallback endpoint should not have been used after a 400")
			_, _ = w.Write([]byte(legacyBody("https://c9.test/a.m4v")))
		},
	}}
	b := newBunkrFallback(t, rt)
	if _, _, err := b.resolveFileURL(context.Background(), "1"); err == nil {
		t.Fatal("expected an error for a 400")
	}
}

// If the prefix is already there it must not be added twice.
func TestBunkrLegacyPrefixNotDoubled(t *testing.T) {
	b := NewBunkr(fallbackCfg()).(*bunkr)
	got, err := b.applyLegacyPrefix("https://c9.test/storage/media/a.m4v", "1")
	if err != nil {
		t.Fatalf("applyLegacyPrefix: %v", err)
	}
	if got != "https://c9.test/storage/media/a.m4v" {
		t.Fatalf("the prefix was doubled: %q", got)
	}
}

// If the signing service returns a Cloudflare challenge it must be classified
// as a CHALLENGE, not a CDN error; otherwise the user is sent to look at the config.
func TestBunkrSignChallengeIsClassified(t *testing.T) {
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"sign.test": forbidden,
	}}
	b := newBunkr(t, rt)
	_, err := b.PrepareURL(context.Background(), "https://c1.test/storage/media/x.mp4")
	if err == nil {
		t.Fatal("expected an error for a challenge")
	}
	if l, ok := LayerOf(err); !ok || l != LayerChallenge {
		t.Errorf("layer = %v, want %v", l, LayerChallenge)
	}
}
