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

// hostRouter, host adına göre yanıt üreten bir RoundTripper.
// Domain rotasyonunu tamamen çevrimdışı test etmeyi sağlıyor: hangi domainin
// 403 döndüğünü, hangisinin TLS hatası verdiğini elle kuruyoruz.
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
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("bilinmeyen host")}
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

// --- URL tanima ---

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
		// LegacyDomains TANINIR (fetch icin kullanilmaz, ayri test).
		"https://bunkr.la/a/ABC123",
		// Joker: listede olmayan yeni bir TLD tanınmali.
		"https://bunkr.xyz/a/ABC123",
		"https://bunkrr.pk/a/ABC123",
		"bunkr.ws/a/ABC123",
	}
	for _, u := range ok {
		if !b.Match(u) {
			t.Errorf("Match(%q) = false, true bekleniyordu", u)
		}
	}
	bad := []string{
		"https://pixeldrain.com/l/abc",
		// GUVENLIK: joker tek etiket karsilar, dusmanca alt alan adi eslesmez.
		"https://bunkr.attacker.com/a/ABC",
		"https://bunkr.ws/",
		"https://bunkr.ws/a/",
		"https://bunkr.ws/x/slug",
		"",
	}
	for _, u := range bad {
		if b.Match(u) {
			t.Errorf("Match(%q) = true, false bekleniyordu", u)
		}
	}
}

// --- XOR cozumu ---

// Anahtar turetimi, sifre cozumunun tek sabit olmayan parcasi: anahtar
// timestamp/3600'den uretiliyor, yani saatlik bir pencereye bagli.
//
// Bu test CANLI DOGRULAMAYA dayaniyor ama canli veriyi GOMMUYOR. 2026-09-09'da
// gercek bir API yaniti uzerinde hem bu uygulama hem bagimsiz bir Python
// uygulamasi ayni URL'i uretti (timestamp 1788991464 -> SECRET_KEY_496942,
// cozulen adres "https://<host>.cdn.cr/<uuid>.mp4" bicimindeydi). Gercek
// base64 ve UUID repoya yazilmiyor: birincisi her cagrida degisiyor, ikincisi
// baskasinin icerigine isaret ediyor. Burada sabitlenen sey FORMUL.
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
			t.Errorf("timestamp %d -> %q, beklenen %q", c.timestamp, got, c.want)
		}
	}

	// Formulun gercek adresi urettigi, gercek anahtarla sentetik bir adres
	// uzerinden dogrulaniyor.
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
		t.Error("bos anahtar hata vermeli")
	}
	if _, err := decryptXOR("bu base64 degil!!!", []byte("k")); err == nil {
		t.Error("gecersiz base64 hata vermeli")
	}
}

// --- Album ayristirma ---

// Alan adlari ve sira, 2026-09-09'da canli bunkr.ws albüm sayfasindan
// dogrulandi: id, name, original, slug, type, extension, size, timestamp,
// thumbnail, cdnEndpoint.
const albumFixture = `<html><head>
<meta property="og:title" content="Tatil Albümü 2026" />
</head><body>
<span class="font-semibold">(3 files)</span>
<script>
window.albumFiles = [
{
  id: 51537490,
  name: "419b02f6-8abe-46aa-bc4a-9fc5b24095cd.mp4",
  original: "Birinci Video - Özgür & Aslı.mp4",
  slug: "birinci-video-abc",
  type: "video/mp4",
  extension: ".mp4",
  size:  1946234880 ,
  timestamp: "12:34:56 20/07/2026",
  thumbnail: "https://i.bunkr.ru/thumbs/birinci.png",
  cdnEndpoint: "https://c3bc-b.cdn.cr"
},
{
  id: 51537491,
  name: "aaa.mp4",
  original: "İkinci { süslü } parantezli.mp4",
  slug: "ikinci-video-def",
  type: "video/mp4",
  extension: ".mp4",
  size:  100 ,
  timestamp: "01:02:03 21/07/2026",
  thumbnail: "https://i.bunkr.ru/thumbs/ikinci.png",
  cdnEndpoint: "https://c3su-b.cdn.cr"
},
{
  id: 51537492,
  name: "bbb.png",
  slug: "ucuncu-resim-ghi",
  type: "image/png",
  extension: ".png",
  size:  bozuk ,
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
		t.Fatalf("%d item, 3 bekleniyordu", len(files))
	}

	if files[0].ID != "51537490" {
		t.Errorf("id = %q", files[0].ID)
	}
	if files[0].Name != "Birinci Video - Özgür & Aslı.mp4" {
		t.Errorf("original adi = %q", files[0].Name)
	}
	if files[0].Slug != "birinci-video-abc" {
		t.Errorf("slug = %q", files[0].Slug)
	}
	if files[0].Size != 1946234880 {
		t.Errorf("size = %d (kaynakta cift bosluk ve virgulden once bosluk var)", files[0].Size)
	}

	// Deger icinde susluk parantez: blok ayirma bozulmamali.
	if files[1].Name != "İkinci { süslü } parantezli.mp4" {
		t.Errorf("susluk parantezli ad bozuldu: %q", files[1].Name)
	}

	// Bozuk size -> -1, item atlanmiyor.
	if files[2].Size != -1 {
		t.Errorf("bozuk size = %d, -1 bekleniyordu", files[2].Size)
	}
	// original yok -> slug + uzanti yedegi.
	if files[2].Name != "ucuncu-resim-ghi.png" {
		t.Errorf("yedek ad = %q", files[2].Name)
	}
}

func TestParseAlbumFilesMissingMarker(t *testing.T) {
	if _, err := parseAlbumFiles("<html>bos sayfa</html>"); err == nil {
		t.Fatal("window.albumFiles yoksa hata bekleniyordu")
	}
}

// Blok ayirma susluk parantez DENGESI uzerinden yapiliyor, satir sonu
// kalibina ("\n},\n") gore degil: bicimlendirme degisirse o kalip sessizce
// tek bir dev item uretir ve album bir dosyaya duser.
func TestSplitJSObjectsHandlesBracesInStrings(t *testing.T) {
	body := `{ a: "i{c}i", b: 1 }, { c: 'x}y', d: 2 }`
	got := splitJSObjects(body)
	if len(got) != 2 {
		t.Fatalf("%d blok, 2 bekleniyordu: %q", len(got), got)
	}
}

func TestSplitJSObjectsSingleLine(t *testing.T) {
	// Tek satira sikistirilmis cikti da calismali.
	body := `{id: 1, original: "a.mp4"},{id: 2, original: "b.mp4"}`
	if got := splitJSObjects(body); len(got) != 2 {
		t.Fatalf("%d blok, 2 bekleniyordu", len(got))
	}
}

// --- Domain rotasyonu ---

func albumHandler(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.URL.RawQuery, "advanced=1") {
		// advanced=1 olmadan albumFiles gelmiyor; bunu sabitliyoruz.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>advanced yok</html>"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(albumFixture))
}

func TestBunkrRotatesPastChallengedDomain(t *testing.T) {
	rt := &hostRouter{
		handlers: map[string]http.HandlerFunc{
			"bunkr.ws":    forbidden,    // Cloudflare challenge
			"bunkr.ac":    albumHandler, // calisan
			"bunkr.black": forbidden,
		},
	}
	b := newBunkr(t, rt)
	body, root, err := b.fetchWithRotation(context.Background(), "/a/X?advanced=1", "")
	if err != nil {
		t.Fatalf("rotasyon basarisiz: %v", err)
	}
	if root != "https://bunkr.ac" {
		t.Errorf("root = %q, bunkr.ac bekleniyordu", root)
	}
	if !strings.Contains(string(body), "window.albumFiles") {
		t.Error("albüm sayfasi gelmedi")
	}
	burned := b.Burned()
	if why, ok := burned["bunkr.ws"]; !ok || !strings.Contains(why, "Cloudflare") {
		t.Errorf("bunkr.ws elenmedi veya neden yanlis: %q", why)
	}
}

// ASIL BULGU: gallery-dl yalnizca 403'te domain eliyor. Olcum, Turkiye agindan
// engelin sertifika hatasi ve baglanti zaman asimi olarak geldigini gosterdi.
// Rotasyon bunlari da kapsamak zorunda, yoksa engelli bir domainde takilir.
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
		t.Fatalf("baglanti hatasinda rotasyon yapilmadi: %v", err)
	}
	if root != "https://bunkr.black" {
		t.Errorf("root = %q", root)
	}
	burned := b.Burned()
	if len(burned) != 2 {
		t.Fatalf("%d domain elendi, 2 bekleniyordu: %v", len(burned), burned)
	}
	for _, d := range []string{"bunkr.ws", "bunkr.ac"} {
		if !strings.Contains(burned[d], "bağlantı kurulamadı") {
			t.Errorf("%s neden = %q", d, burned[d])
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
		t.Fatalf("ErrAllDomainsBurned bekleniyordu: %v", err)
	}
	// Teshis icin neden kaydedilmis olmali.
	if l, ok := LayerOf(err); !ok || l != LayerChallenge {
		t.Errorf("Layer = %v, %v bekleniyordu", l, LayerChallenge)
	}
}

// Kalici hata rotasyonu TETIKLEMEMELI: 404 domainin saglam oldugunu ama
// icerigin olmadigini soyler. Her domaini denemek 14 bos istek demek.
func TestBunkrPermanentErrorDoesNotRotate(t *testing.T) {
	notFound := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"bunkr.ws": notFound, "bunkr.ac": albumHandler, "bunkr.black": albumHandler,
	}}
	b := newBunkr(t, rt)
	if _, _, err := b.fetchWithRotation(context.Background(), "/a/X", ""); err == nil {
		t.Fatal("404 hata vermeliydi")
	}
	if len(b.Burned()) != 0 {
		t.Errorf("404'te domain elendi: %v", b.Burned())
	}
	if len(rt.seen) != 1 {
		t.Errorf("%d istek atildi, 1 bekleniyordu: %v", len(rt.seen), rt.seen)
	}
}

// Joker girdi rotasyon havuzuna GIRMEMELI: "bunkr.*" adresine istek atilamaz.
func TestBunkrWildcardNotInRotationPool(t *testing.T) {
	cfg := bunkrCfg()
	cfg.Domains = append(cfg.Domains, "bunkr.*")
	b := NewBunkr(cfg).(*bunkr)
	for _, d := range b.roots() {
		if strings.ContainsRune(d, '*') {
			t.Fatalf("joker rotasyon havuzunda: %q", d)
		}
	}
}

// --- Uctan uca cozumleme ---

func apiHandler(t *testing.T, wantReferer string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("API metodu = %s, POST bekleniyordu", r.Method)
		}
		// Referer ve Origin ZORUNLU: endpoint bunlari kontrol ediyor.
		if got := r.Header.Get("Referer"); !strings.HasPrefix(got, wantReferer) {
			t.Errorf("API Referer = %q, %q ile baslamaliydi", got, wantReferer)
		}
		if got := r.Header.Get("Origin"); got != "https://get.test" {
			t.Errorf("API Origin = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://cdn.test/dosya.mp4","encrypted":false,"timestamp":0}`))
	}
}

// signHandler, imza servisini taklit eder. Gercek servis gibi yalnizca yola
// bakip token uretiyor.
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
		t.Fatalf("beklenmeyen item hatalari: %v", itemErrs)
	}
	if len(items) != 3 {
		t.Fatalf("%d item, 3 bekleniyordu", len(items))
	}

	if items[0].Dir != "Tatil Albümü 2026" {
		t.Errorf("Dir = %q", items[0].Dir)
	}
	if items[0].Filename != "Birinci Video - Özgür & Aslı.mp4" {
		t.Errorf("Filename = %q", items[0].Filename)
	}
	// URL IMZALI olmak zorunda: imzasiz adres CDN'de 403 aliyor.
	if !strings.HasPrefix(items[0].URL, "https://cdn.test/dosya.mp4?") {
		t.Errorf("URL = %q", items[0].URL)
	}
	if !strings.Contains(items[0].URL, "token=") || !strings.Contains(items[0].URL, "ex=") {
		t.Errorf("URL imzasiz: %q", items[0].URL)
	}
	// SourcePage slug uzerinden kurulmali: ResolveOne buna bagli.
	if items[0].SourcePage != "https://bunkr.ws/f/birinci-video-abc" {
		t.Errorf("SourcePage = %q", items[0].SourcePage)
	}
	// Indirme Referer'i POLITIKADAN degil PROTOKOLDEN geliyor; referer_policy
	// "none" olmasina ragmen dolu olmak zorunda.
	if ref := items[0].Headers["Referer"]; ref != "https://get.test/file/51537490" {
		t.Errorf("indirme Referer = %q", ref)
	}
	if items[0].Size != 1946234880 {
		t.Errorf("Size = %d", items[0].Size)
	}
	// bunkr sha256 vermiyor.
	if items[0].SHA256 != "" {
		t.Errorf("SHA256 dolu geldi: %q", items[0].SHA256)
	}
}

// Tek item'in API cagrisi duserse album DUSMEMELI, ItemError toplanmali.
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
		t.Fatalf("album dusmemeliydi: %v", err)
	}
	if n != 2 {
		t.Errorf("%d item yield edildi, 2 bekleniyordu", n)
	}
	if len(itemErrs) != 1 {
		t.Fatalf("%d item hatasi, 1 bekleniyordu", len(itemErrs))
	}
	if l, ok := LayerOf(itemErrs[0].Err); !ok || l != LayerItemPage {
		t.Errorf("Layer = %v, %v bekleniyordu", l, LayerItemPage)
	}
}

func TestBunkrResolveMediaAndResolveOne(t *testing.T) {
	mediaPage := `<html><head><meta property="og:title" content="Tek Dosya.mp4" /></head>
<body><div data-file-id="99887766"></div></body></html>`
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"bunkr.ws": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(mediaPage))
		},
		"api.test":  apiHandler(t, "https://get.test/file/99887766"),
		"sign.test": signHandler,
	}}
	b := newBunkr(t, rt)

	got, err := b.ResolveOne(context.Background(), "https://bunkr.ws/f/tek-dosya")
	if err != nil {
		t.Fatalf("ResolveOne: %v", err)
	}
	if got.Filename != "Tek Dosya.mp4" {
		t.Errorf("Filename = %q", got.Filename)
	}
	if got.Headers["Referer"] != "https://get.test/file/99887766" {
		t.Errorf("Referer = %q", got.Headers["Referer"])
	}
}

func TestBunkrResolveOneRejectsAlbum(t *testing.T) {
	b := NewBunkr(bunkrCfg())
	if _, err := b.ResolveOne(context.Background(), "https://bunkr.ws/a/ABC"); err == nil {
		t.Fatal("albüm URL'i icin hata bekleniyordu")
	}
}

// --- Bakim placeholder'i ---

// bunkr silinen/bakimdaki dosyalar icin 404 DEGIL, 200 ile placeholder video
// donduruyor. Durum kodu temiz, icerik cop. Bu kontrol olmadan arac copu
// "basariyla indirdim" sayar.
func TestBunkrValidateResponseCatchesMaintenance(t *testing.T) {
	b := NewBunkr(bunkrCfg()).(*bunkr)

	for _, name := range []string{"/maint.mp4", "/maintenance-vid.mp4"} {
		req, _ := http.NewRequest(http.MethodGet, "https://cdn.test"+name, nil)
		resp := &http.Response{StatusCode: 200, Request: req, Header: http.Header{}}
		err := b.ValidateResponse(resp)
		if err == nil {
			t.Fatalf("%s bakim placeholder'i yakalanmadi", name)
		}
		if l, ok := LayerOf(err); !ok || l != LayerCDN {
			t.Errorf("Layer = %v, %v bekleniyordu", l, LayerCDN)
		}
	}

	req, _ := http.NewRequest(http.MethodGet, "https://cdn.test/gercek-dosya.mp4", nil)
	resp := &http.Response{StatusCode: 200, Request: req, Header: http.Header{}}
	if err := b.ValidateResponse(resp); err != nil {
		t.Errorf("gercek dosya reddedildi: %v", err)
	}
}

func TestBunkrClassifyStatus(t *testing.T) {
	b := NewBunkr(bunkrCfg()).(*bunkr)

	// Challenge kendi katmanina gitmeli.
	cf := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	cf.Header.Set("CF-Mitigated", "challenge")
	err := b.ClassifyStatus(cf, nil)
	if l, ok := LayerOf(err); !ok || l != LayerChallenge {
		t.Errorf("challenge Layer = %v", l)
	}

	// Challenge olmayan 403: imzali URL suresi dolmus olabilir, indirici
	// ResolveOne ile yeniden cozsun -> nil donmeli.
	plain := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	if err := b.ClassifyStatus(plain, []byte("<h1>403 Forbidden</h1>")); err != nil {
		t.Errorf("duz 403 nil donmeliydi: %v", err)
	}

	// 403 disi dokunulmamali.
	other := &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}}
	if err := b.ClassifyStatus(other, nil); err != nil {
		t.Errorf("500 nil donmeliydi: %v", err)
	}
}

// --- Yonlendirme ---

// Otomatik yonlendirme takibi KAPALI olmak zorunda: Cloudflare challenge'i bir
// yonlendirme olarak da gelebiliyor ve otomatik takip o sinyali yutar.
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
		t.Fatalf("yonlendirme izlenemedi: %v", err)
	}
	if hops != 2 {
		t.Errorf("%d istek, 2 bekleniyordu", hops)
	}
	if !strings.Contains(string(body), "window.albumFiles") {
		t.Error("hedef sayfa gelmedi")
	}
}

func TestBunkrRedirectLoopIsBounded(t *testing.T) {
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"bunkr.ws": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/dongu")
			w.WriteHeader(http.StatusFound)
		},
	}}
	cfg := bunkrCfg()
	cfg.Domains = []string{"bunkr.ws"}
	cfg.HTTPClient = &http.Client{Transport: rt}
	b := NewBunkr(cfg).(*bunkr)
	if _, _, err := b.fetchWithRotation(context.Background(), "/dongu", ""); err == nil {
		t.Fatal("sonsuz yonlendirme durdurulmali")
	}
}

// --- Imzali indirme akisi (2026-09-10) ---

// OLCULDU: CDN imzasiz GET'i dosyaya HIC BAKMADAN reddediyor. Var olan dosya
// ile uydurma bir ad icin yanit bayt bayt ayni (403, ayni govde, ayni
// basliklar). Bu yuzden imza zorunlu ve 403'e bakip "dosya silinmis" demek
// yanlis olurdu.
func TestBunkrSignsCurrentAPIShape(t *testing.T) {
	var signedPath string
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"api.test": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"mediafiles":"https://c1.test","path":"/storage/media/video-abc.mp4","original":"Gerçek Ad.mp4"}`))
		},
		"sign.test": func(w http.ResponseWriter, r *http.Request) {
			signedPath = r.URL.Query().Get("path")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"abc123","ex":1789054914}`))
		},
	}}
	b := newBunkr(t, rt)

	got, _, err := b.resolveFileURL(context.Background(), "555")
	if err != nil {
		t.Fatalf("resolveFileURL: %v", err)
	}

	// Imza servisine yolun COZULMUS hali gitmeli.
	if signedPath != "/storage/media/video-abc.mp4" {
		t.Errorf("imzalanan yol = %q", signedPath)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("uretilen URL ayristirilamadi: %v", err)
	}
	if u.Host != "c1.test" || u.Path != "/storage/media/video-abc.mp4" {
		t.Errorf("adres yanlis kuruldu: %q", got)
	}
	q := u.Query()
	if q.Get("token") != "abc123" {
		t.Errorf("token = %q", q.Get("token"))
	}
	if q.Get("ex") != "1789054914" {
		t.Errorf("ex = %q", q.Get("ex"))
	}
	// n, CDN'in Content-Disposition'da kullandigi ozgun ad.
	if q.Get("n") != "Gerçek Ad.mp4" {
		t.Errorf("n = %q", q.Get("n"))
	}
}

// Eski bicim (XOR ile sifreli tam adres) hala calismali; o da imzalanmali.
func TestBunkrSignsLegacyAPIShape(t *testing.T) {
	plain := "https://c9.test/eski/dosya.mp4"
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

	got, _, err := b.resolveFileURL(context.Background(), "42")
	if err != nil {
		t.Fatalf("resolveFileURL: %v", err)
	}
	if !strings.HasPrefix(got, plain+"?") {
		t.Errorf("eski bicim adresi bozuldu: %q", got)
	}
	if !strings.Contains(got, "token=") {
		t.Errorf("eski bicim imzalanmadi: %q", got)
	}
}

// Imza servisi dusrse indirme adresi UYDURULMAMALI: imzasiz adres nasilsa
// 403 alir ve hata "403" olarak gorunup teshisi saptirir.
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

	if _, _, err := b.resolveFileURL(context.Background(), "7"); err == nil {
		t.Fatal("imza servisi 500 dondugunde hata bekleniyordu")
	} else if l, ok := LayerOf(err); !ok || l != LayerCDN {
		t.Errorf("katman = %v, %v bekleniyordu", l, LayerCDN)
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

	if _, _, err := b.resolveFileURL(context.Background(), "7"); err == nil {
		t.Fatal("token'siz imza yaniti hata vermeliydi")
	}
}

// API ne yeni ne eski bicimde adres vermezse net hata.
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
		t.Fatal("bos yanit icin hata bekleniyordu")
	}
}

// mediafiles sonunda "/" olsa bile yol ikiye katlanmamali.
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
		t.Errorf("adres = %q", got)
	}
}
