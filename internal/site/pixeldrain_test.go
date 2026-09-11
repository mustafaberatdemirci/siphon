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

		// Joker: gallery-dl ve cyberdrop-dl bunkr icin "bunkr.*" kullaniyor.
		{"bunkr.cr", []string{"bunkr.*"}, true},
		{"bunkr.ws", []string{"bunkr.*"}, true},
		{"notbunkr.cr", []string{"bunkr.*"}, false},
		{"bunkr.", []string{"bunkr.*"}, false},
		{"bunkr", []string{"bunkr.*"}, false},

		// GUVENLIK SINIRI: joker TEK etiket karsilar. Coklu etiket olsaydi
		// girdi listesindeki dusmanca bir link guvenilen site sayilirdi.
		{"bunkr.attacker.com", []string{"bunkr.*"}, false},
		{"bunkr.evil.example.org", []string{"bunkr.*"}, false},
		{"bunkr.com.phish.ru", []string{"bunkr.*"}, false},
		// Bunun bedeli: cok parcali TLD acikca listelenmek zorunda.
		{"bunkr.co.uk", []string{"bunkr.*"}, false},
		{"bunkr.co.uk", []string{"bunkr.co.uk"}, true},

		{"cdn.bunkr.la", []string{"*.bunkr.la"}, true},
		{"a.b.bunkr.la", []string{"*.bunkr.la"}, false},
		{"bunkr.la", []string{"*.bunkr.la"}, false},

		// Desende sema/yol verilmisse temizlenir.
		{"pixeldrain.com", []string{"https://pixeldrain.com/"}, true},
	}
	for _, c := range cases {
		if got := MatchHost(c.host, c.patterns); got != c.want {
			t.Errorf("MatchHost(%q, %v) = %v, beklenen %v", c.host, c.patterns, got, c.want)
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

		// Kisa link: pixeldra.in/{id}
		{"https://pixeldra.in/qwerty", refFile, "qwerty", false},
		{"pixeldra.in/qwerty", refFile, "qwerty", false},

		// Alternatif TLD'ler; yedisi de canli olculdu.
		{"https://pixeldrain.tech/l/abc123", refAlbum, "abc123", false},
		{"https://pixeldrain.biz/u/xyz789", refFile, "xyz789", false},

		// LegacyDomains TANINIR ama fetch icin kullanilmaz (asagida ayri test).
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
				t.Errorf("parse(%q) hata bekleniyordu, %+v geldi", c.in, got)
			}
			if p.Match(c.in) {
				t.Errorf("Match(%q) = true, false bekleniyordu", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parse(%q) beklenmeyen hata: %v", c.in, err)
			continue
		}
		if got.kind != c.wantKind || got.id != c.wantID {
			t.Errorf("parse(%q) = {%v %q}, beklenen {%v %q}", c.in, got.kind, got.id, c.wantKind, c.wantID)
		}
		if !p.Match(c.in) {
			t.Errorf("Match(%q) = false, true bekleniyordu", c.in)
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
		t.Fatal("olu domain fetch icin kullanildi; LegacyDomains ayrimi calismiyor")
	}
	if host != "pixeldrain.com" {
		t.Fatalf("fetchHost = %q, aktif listenin ilki bekleniyordu", host)
	}
}

// rewriteTransport, tum istekleri test sunucusuna yonlendirir.
// Dokumanin belirttigi TEK test enjeksiyon mekanizmasi bu.
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
		t.Fatalf("test sunucu URL: %v", err)
	}
	rt := &rewriteTransport{target: u}
	cfg := testCfg()
	cfg.HTTPClient = &http.Client{Transport: rt}
	return &pixeldrain{cfg: cfg}, rt
}

const albumJSON = `{
  "success": true,
  "id": "abc123",
  "title": "Tatil 2026",
  "file_count": 3,
  "files": [
    {"success": true, "id": "f1", "name": "bir.jpg",  "size": 100, "mime_type": "image/jpeg"},
    {"success": true, "id": "f2", "name": "iki.mp4",  "size": 200, "mime_type": "video/mp4"},
    {"success": true, "id": "f3", "name": "uc.png",   "size": 0,   "mime_type": "image/png"}
  ]
}`

func TestResolveAlbumUsesSingleRequest(t *testing.T) {
	p, rt := newTestResolver(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/list/") {
			t.Errorf("beklenmeyen yol: %s", r.URL.Path)
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
		t.Fatalf("beklenmeyen item hatalari: %v", itemErrs)
	}
	if len(items) != 3 {
		t.Fatalf("%d item, 3 bekleniyordu", len(items))
	}

	// Asil kriter: 200 dosyalik albumde 201 istek atma kurali.
	if len(rt.seen) != 1 {
		t.Fatalf("%d istek atildi (%v), 1 bekleniyordu; per-file /info cagrisi rate limit tetikler",
			len(rt.seen), rt.seen)
	}

	if items[0].Dir != "Tatil 2026" {
		t.Errorf("Dir = %q", items[0].Dir)
	}
	if items[0].Filename != "bir.jpg" || items[0].Index != 0 {
		t.Errorf("ilk item yanlis: %+v", items[0])
	}
	if items[1].SourcePage != "https://pixeldrain.com/u/f2" {
		t.Errorf("SourcePage = %q; yeniden cozumleme icin zorunlu", items[1].SourcePage)
	}
	if !strings.Contains(items[1].URL, "/api/file/f2") {
		t.Errorf("URL = %q", items[1].URL)
	}
	// Boyut bilinmiyorsa -1, 0 degil.
	if items[2].Size != -1 {
		t.Errorf("bilinmeyen boyut = %d, -1 bekleniyordu", items[2].Size)
	}
	// Gercek /list yaniti hash_sha256 tasiyor (canli dogrulandi: 34/34).
	// Fixture'da alan yok, ama dolu geldiginde Item'a tasinmak zorunda.
	if items[0].SHA256 != "" {
		t.Errorf("fixture'da hash yok, SHA256 bos olmaliydi: %q", items[0].SHA256)
	}
}

func TestResolveSingleFileHasSHA256(t *testing.T) {
	p, _ := newTestResolver(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"id":"xyz789","name":"tek.bin","size":42,"hash_sha256":"deadbeef"}`))
	})
	var got Item
	if _, err := p.Resolve(context.Background(), "https://pixeldrain.com/u/xyz789",
		func(it Item) error { got = it; return nil }); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.SHA256 != "deadbeef" {
		t.Errorf("SHA256 = %q; tekil dosya yolunda dolu olmali", got.SHA256)
	}
	if got.Size != 42 || got.Filename != "tek.bin" {
		t.Errorf("item yanlis: %+v", got)
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
		t.Fatal("hata bekleniyordu")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("APIError bekleniyordu, %T geldi: %v", err, err)
	}
	if apiErr.Value != "transfer_limit_exceeded" {
		t.Errorf("Value = %q", apiErr.Value)
	}
	if !apiErr.Retryable() {
		t.Error("transfer_limit_exceeded retryable olmali")
	}
	if apiErr.CaptchaRequired() {
		t.Error("transfer_limit_exceeded captcha istemiyor")
	}
	if l, ok := LayerOf(err); !ok || l != LayerFetch {
		t.Errorf("Layer = %v, %v bekleniyordu", l, LayerFetch)
	}
}

func TestCaptchaCodeIsNotRetryable(t *testing.T) {
	e := &APIError{Status: 403, Value: "file_rate_limited_captcha_required"}
	if e.Retryable() {
		t.Error("captcha isteyen kod retryable olmamali; beklemek cozmez")
	}
	if !e.CaptchaRequired() {
		t.Error("CaptchaRequired false dondu")
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
		t.Fatal("hata bekleniyordu")
	}
	l, ok := LayerOf(err)
	if !ok || l != LayerChallenge {
		t.Fatalf("Layer = %v, %v bekleniyordu; challenge TLS'ten ayri olmali", l, LayerChallenge)
	}
}

func TestResolveOneRejectsAlbumURL(t *testing.T) {
	p := &pixeldrain{cfg: testCfg()}
	if _, err := p.ResolveOne(context.Background(), "https://pixeldrain.com/l/abc123"); err == nil {
		t.Fatal("albüm URL'i icin hata bekleniyordu")
	}
}

func TestRegistryBuildsResolver(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(PixeldrainName, NewPixeldrain); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Register(PixeldrainName, NewPixeldrain); err == nil {
		t.Error("ayni isimle ikinci kayit hata vermeli")
	}
	rs, err := reg.Build([]SiteConfig{{Name: PixeldrainName, Domains: []string{"pixeldrain.com"}}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(rs) != 1 || !rs[0].Match("https://pixeldrain.com/l/abc") {
		t.Fatal("resolver kurulamadi veya Match calismadi")
	}
	if _, err := reg.Build([]SiteConfig{{Name: "yok"}}); err == nil {
		t.Error("kayitsiz site icin hata bekleniyordu")
	}
}

func TestWithDefaults(t *testing.T) {
	c := SiteConfig{}.WithDefaults()
	if c.MaxRetries != DefaultMaxRetries || c.BaseDelay != DefaultBaseDelay ||
		c.MaxDelay != DefaultMaxDelay || c.MaxElapsed != DefaultMaxElapsed ||
		c.MaxConcurrent != DefaultMaxConcurrent || c.RefererPolicy != RefererNone {
		t.Fatalf("varsayilanlar eksik: %+v", c)
	}
	// Verilen deger ezilmemeli.
	c2 := SiteConfig{MaxRetries: 1, RefererPolicy: RefererItemPage}.WithDefaults()
	if c2.MaxRetries != 1 || c2.RefererPolicy != RefererItemPage {
		t.Fatalf("verilen deger ezildi: %+v", c2)
	}
}

// Gercek pixeldrain /list yaniti, resmi API belgesinin aksine, files[] icinde
// dolu hash_sha256 donduruyor. Bu testin varligi o davranisi sabitliyor:
// dolu gelen hash Item'a tasinmak ve indiricide dogrulanmak zorunda.
func TestAlbumCarriesSHA256WhenPresent(t *testing.T) {
	const withHash = `{"success":true,"id":"a1","title":"Albüm","file_count":1,
	  "files":[{"success":true,"id":"f1","name":"bir.bin","size":10,
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
		t.Fatalf("album item SHA256 tasinmadi: %q", got.SHA256)
	}
}

// Bulgu 4: file_count ile files[] ayrisirsa liste eksik geldi demektir.
// Sessizce daha az dosya indirip cikis 0 vermek kabul edilemez.
func TestAlbumFileCountMismatchIsReported(t *testing.T) {
	const short = `{"success":true,"id":"a1","title":"Albüm","file_count":5,
	  "files":[{"success":true,"id":"f1","name":"bir.bin","size":10}]}`
	p, _ := newTestResolver(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(short))
	})
	var n int
	itemErrs, err := p.Resolve(context.Background(), "https://pixeldrain.com/l/a1",
		func(Item) error { n++; return nil })
	if err != nil {
		t.Fatalf("albüm düşmemeli: %v", err)
	}
	if n != 1 {
		t.Fatalf("%d item, 1 bekleniyordu", n)
	}
	if len(itemErrs) == 0 {
		t.Fatal("file_count uyusmazligi bildirilmedi; cikis kodu sessizce 0 olurdu")
	}
	if l, ok := LayerOf(itemErrs[0].Err); !ok || l != LayerParse {
		t.Errorf("Layer = %v, %v bekleniyordu", l, LayerParse)
	}
}

// Bulgu 11: referer_policy indirme istegine yansimak zorunda.
// pixeldrain'de politika "none" oldugu icin bos; ama mekanizma dogru yerde
// olmali cunku bunkr item sayfasi Referer'ini transferde zorunlu kiliyor.
func TestItemHeadersFollowRefererPolicy(t *testing.T) {
	const one = `{"success":true,"id":"f1","name":"bir.bin","size":10}`
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
		t.Errorf("none politikasinda Referer gonderilmemeli: %v", h.Headers)
	}
	if h := run(RefererItemPage); h.Headers["Referer"] != "https://pixeldrain.com/u/f1" {
		t.Errorf("item_page Referer = %q, item sayfasi bekleniyordu", h.Headers["Referer"])
	}
	if h := run(RefererOrigin); h.Headers["Referer"] != "https://pixeldrain.com/" {
		t.Errorf("origin Referer = %q", h.Headers["Referer"])
	}
}

// Bulgu 5: 403 her zaman "imzali URL suresi doldu" degil. pixeldrain rate
// limit ve captcha durumlarini da 403 ile bildiriyor; siniflandirici bunlari
// ayirmak zorunda, yoksa arac rate limitliyken yeniden cozup tekrar dener.
func TestClassifyStatusSeparatesRateLimitFromExpiredURL(t *testing.T) {
	p := &pixeldrain{cfg: testCfg()}

	resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	body := []byte(`{"success":false,"value":"transfer_limit_exceeded","message":"limit"}`)
	err := p.ClassifyStatus(resp, body)
	if err == nil {
		t.Fatal("taninabilir zarf nil dondu; dl varsayilani uygulanir ve 403 expired sayilir")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Value != "transfer_limit_exceeded" {
		t.Fatalf("APIError bekleniyordu: %v", err)
	}
	if l, ok := LayerOf(err); !ok || l != LayerFetch {
		t.Errorf("Layer = %v", l)
	}

	// Cloudflare challenge kendi katmanina gitmeli.
	cf := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	cf.Header.Set("CF-Mitigated", "challenge")
	cerr := p.ClassifyStatus(cf, []byte(`{"success":false,"value":"blocked","message":"x"}`))
	if l, ok := LayerOf(cerr); !ok || l != LayerChallenge {
		t.Errorf("challenge Layer = %v, %v bekleniyordu", l, LayerChallenge)
	}

	// Taninmayan govde: nil donmeli ki dl "imzali URL suresi doldu" yolunu izlesin.
	opaque := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	if err := p.ClassifyStatus(opaque, []byte("<html>Access Denied</html>")); err != nil {
		t.Errorf("taninmayan govde nil donmeli, %v geldi", err)
	}
}

// Resolver, site.StatusClassifier arayuzunu gercekten karsiliyor mu?
// Karsilamazsa main.go'daki type assertion sessizce atlar ve bulgu 5 geri doner.
func TestPixeldrainImplementsStatusClassifier(t *testing.T) {
	var r Resolver = NewPixeldrain(testCfg())
	if _, ok := r.(StatusClassifier); !ok {
		t.Fatal("pixeldrain StatusClassifier arayuzunu karsilamiyor")
	}
}

// --- Ucretli hesap API anahtari ---

// pixeldrain'in belgesi: hotlink yalnizca ucretli hesapla serbest ve sinir
// DOSYA BASINA bir sayac (indirme > 3 x goruntulenme). Tasarlanmis cikis yolu
// API anahtari: HTTP Basic, kullanici adi bos, parola anahtar. Hem API
// cagrisina hem TRANSFER istegine gitmeli; sinir asil transferde biniyor.
func TestPixeldrainAPIKeyIsSentAsBasicAuth(t *testing.T) {
	var gotAuth string
	p, _ := newTestResolver(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"id":"xyz789","name":"tek.bin","size":42}`))
	})
	p.apiKey = "gizli-anahtar"

	var it Item
	if _, err := p.Resolve(context.Background(), "https://pixeldrain.com/u/xyz789",
		func(i Item) error { it = i; return nil }); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Belgedeki bicim: Basic base64(":" + api_key)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(":gizli-anahtar"))
	if gotAuth != want {
		t.Errorf("API cagrisinda Authorization = %q, %q bekleniyordu", gotAuth, want)
	}
	if it.Headers["Authorization"] != want {
		t.Errorf("transfer basliginda Authorization = %q; sinir asil burada biniyor", it.Headers["Authorization"])
	}
}

// Anahtar yoksa hicbir Authorization basligi gitmemeli: bos Basic gondermek
// anonim istegi "kimlik dogrulama basarisiz" durumuna dusurebilir.
func TestPixeldrainNoAPIKeyMeansNoAuthHeader(t *testing.T) {
	var gotAuth string
	p, _ := newTestResolver(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"id":"xyz789","name":"tek.bin","size":42}`))
	})
	var it Item
	_, _ = p.Resolve(context.Background(), "https://pixeldrain.com/u/xyz789",
		func(i Item) error { it = i; return nil })
	if gotAuth != "" {
		t.Errorf("anahtarsiz Authorization gitti: %q", gotAuth)
	}
	if _, ok := it.Headers["Authorization"]; ok {
		t.Error("anahtarsiz transfer basliginda Authorization var")
	}
}

// Anahtar config'ten (Extra) okunmali; bosluklar kirpilmali.
func TestPixeldrainAPIKeyFromConfig(t *testing.T) {
	cfg := testCfg()
	cfg.Extra = map[string]string{ExtraPixeldrainAPIKey: "  abc123  "}
	p := NewPixeldrain(cfg).(*pixeldrain)
	if p.apiKey != "abc123" {
		t.Errorf("apiKey = %q", p.apiKey)
	}
}
