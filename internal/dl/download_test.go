package dl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

var payload = bytes.Repeat([]byte("siphon-veri-blogu."), 4096) // ~72 KB

func payloadSHA() string {
	s := sha256.Sum256(payload)
	return hex.EncodeToString(s[:])
}

type capture struct {
	mu       []http.Header
	statuses []int
}

// rangeServer, Range/If-Range semantigini Go'nun kendi ServeContent'ine
// birakir: elle yazilmis bir taklit yerine gercek davranis test edilir.
func rangeServer(t *testing.T, etag string, cap *capture) *httptest.Server {
	t.Helper()
	modtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cap != nil {
			cap.mu = append(cap.mu, r.Header.Clone())
		}
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		http.ServeContent(w, r, "veri.bin", modtime, bytes.NewReader(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testItem(url, name string) site.Item {
	return site.Item{
		URL:        url,
		SourcePage: "https://ornek.test/u/abc",
		Filename:   name,
		Size:       int64(len(payload)),
	}
}

func readState(t *testing.T, path string) State {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state okunamadi: %v", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("state cozulemedi: %v", err)
	}
	return st
}

func TestFreshDownloadWritesFileAndCleansUp(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	it := testItem(srv.URL+"/veri.bin", "veri.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("Download: %v", err)
	}

	final := filepath.Join(out, "veri.bin")
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("nihai dosya yok: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("icerik yanlis: %d bayt, %d bekleniyordu", len(got), len(payload))
	}
	// Yarim dosya asla nihai adla diskte durmaz; .part ve .state temizlenir.
	for _, leftover := range []string{final + ".part", final + ".part.state"} {
		if _, err := os.Stat(leftover); err == nil {
			t.Errorf("artik dosya kaldi: %s", leftover)
		}
	}
}

func TestSHA256MismatchDoesNotRename(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	it := testItem(srv.URL+"/veri.bin", "veri.bin")
	it.SHA256 = strings.Repeat("00", 32)
	_, err := d.Download(context.Background(), out, it)
	if !errors.Is(err, ErrSHA256Mismatch) {
		t.Fatalf("ErrSHA256Mismatch bekleniyordu, %v geldi", err)
	}
	if _, err := os.Stat(filepath.Join(out, "veri.bin")); err == nil {
		t.Fatal("hash tutmadigi halde dosya nihai adla yazildi")
	}
}

// slowServer, ilk N baytı verip bloklar. Ctrl+C senaryosunu taklit eder.
func slowServer(t *testing.T, firstChunk int, release <-chan struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload[:firstChunk])
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestInterruptThenResumeProducesCorrectHash(t *testing.T) {
	release := make(chan struct{})
	slow := slowServer(t, 20000, release)
	out := tempDir(t)

	ctx, cancel := context.WithCancel(context.Background())
	d := &Downloader{Client: slow.Client()}
	it := testItem(slow.URL+"/veri.bin", "veri.bin")
	it.SHA256 = payloadSHA()

	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err := d.Download(ctx, out, it)
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("context.Canceled bekleniyordu, %v geldi", err)
	}

	part := filepath.Join(out, "veri.bin.part")
	statePath := part + ".state"
	st := readState(t, statePath)
	if st.Offset <= 0 || st.Offset >= int64(len(payload)) {
		t.Fatalf("beklenmeyen offset: %d", st.Offset)
	}
	if st.ValidatorType != ValidatorETag || st.Validator != `"v1"` {
		t.Fatalf("validator yanlis: %+v", st)
	}
	if len(st.SHA256State) == 0 {
		t.Fatal("sha256 durumu kaydedilmedi; resume'da hash bastan hesaplanmak zorunda kalir")
	}
	fi, err := os.Stat(part)
	if err != nil || fi.Size() != st.Offset {
		t.Fatalf(".part boyutu state ile uyusmuyor: %v", err)
	}

	// Ayni klasorde, calisan bir sunucuyla devam et.
	cap := &capture{}
	srv := rangeServer(t, `"v1"`, cap)
	d2 := &Downloader{Client: srv.Client()}
	it2 := testItem(srv.URL+"/veri.bin", "veri.bin")
	it2.SHA256 = payloadSHA()
	if _, err := d2.Download(context.Background(), out, it2); err != nil {
		t.Fatalf("resume: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(out, "veri.bin"))
	if err != nil {
		t.Fatalf("nihai dosya: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("resume sonrasi icerik bozuk")
	}
	if len(cap.mu) == 0 || cap.mu[0].Get("Range") == "" {
		t.Fatal("resume isteginde Range basligi yok")
	}
	if cap.mu[0].Get("If-Range") != `"v1"` {
		t.Errorf("If-Range = %q, guclu ETag bekleniyordu", cap.mu[0].Get("If-Range"))
	}
}

func TestWeakETagFallsBackToLastModified(t *testing.T) {
	release := make(chan struct{})
	// Zayif ETag: W/ onekli. If-Range'de KULLANILAMAZ (RFC 7232).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"zayif"`)
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2026 03:04:05 GMT")
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload[:10000])
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(srv.Close)

	out := tempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	d := &Downloader{Client: srv.Client()}
	_, _ = d.Download(ctx, out, testItem(srv.URL+"/veri.bin", "veri.bin"))
	close(release)

	st := readState(t, filepath.Join(out, "veri.bin.part.state"))
	if st.ValidatorType != ValidatorLastModified {
		t.Fatalf("validator_type = %q, zayif ETag'de Last-Modified'a dusmeliydi", st.ValidatorType)
	}
	if strings.HasPrefix(st.Validator, "W/") {
		t.Fatal("zayif ETag validator olarak kaydedildi; bozuk dosya uretir")
	}
}

func TestNoValidatorMeansNoResumeAttempt(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ne ETag ne Last-Modified.
		w.Header()["Last-Modified"] = nil
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload[:10000])
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(srv.Close)

	out := tempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	d := &Downloader{Client: srv.Client()}
	_, _ = d.Download(ctx, out, testItem(srv.URL+"/veri.bin", "veri.bin"))
	close(release)

	st := readState(t, filepath.Join(out, "veri.bin.part.state"))
	if st.resumable() {
		t.Fatalf("validator yokken resume denenebilir isaretlendi: %+v", st)
	}

	cap := &capture{}
	srv2 := rangeServer(t, "", cap)
	d2 := &Downloader{Client: srv2.Client()}
	if _, err := d2.Download(context.Background(), out, testItem(srv2.URL+"/veri.bin", "veri.bin")); err != nil {
		t.Fatalf("ikinci kosu: %v", err)
	}
	if cap.mu[0].Get("Range") != "" {
		t.Fatal("validator yokken Range gonderildi; iki farkli dosyanin parcalari yapistirilabilir")
	}
	got, _ := os.ReadFile(filepath.Join(out, "veri.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("bastan indirme sonrasi icerik bozuk")
	}
}

func TestServerReturns200OnResumeResetsFile(t *testing.T) {
	out := tempDir(t)
	part := filepath.Join(out, "veri.bin.part")
	// Elle bozuk bir .part + state kur: sunucu Range'i yok sayip 200 donecek.
	if err := os.WriteFile(part, []byte("eski-icerik"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := State{Offset: 11, Validator: `"eski"`, ValidatorType: ValidatorETag, TotalSize: int64(len(payload))}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(part+".state", data, 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Range'i kasitli olarak yok say.
		w.Header().Set("ETag", `"yeni"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/veri.bin", "veri.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(out, "veri.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("200 sonrasi eski icerik temizlenmedi")
	}
}

func TestRange416WhenAlreadyComplete(t *testing.T) {
	out := tempDir(t)
	part := filepath.Join(out, "veri.bin.part")
	// .part tam boyutta ama rename dusmus.
	if err := os.WriteFile(part, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	h.Write(payload)
	st := State{
		Offset: int64(len(payload)), Validator: `"v1"`, ValidatorType: ValidatorETag,
		TotalSize: int64(len(payload)),
	}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(part+".state", data, 0o644); err != nil {
		t.Fatal(err)
	}

	srv := rangeServer(t, `"v1"`, nil) // ServeContent bu Range'e 416 doner
	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/veri.bin", "veri.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("416/tamamlanmis yolu: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(out, "veri.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("tamamlanmis .part rename edilmedi")
	}
}

func TestRange416WhenPartIsCorruptResets(t *testing.T) {
	out := tempDir(t)
	part := filepath.Join(out, "veri.bin.part")
	// State "tamamlandi" demiyor: offset != total. 416 burada "bozuk" demek.
	if err := os.WriteFile(part, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	st := State{
		Offset: int64(len(payload)), Validator: `"v1"`, ValidatorType: ValidatorETag,
		TotalSize: int64(len(payload)) + 999, // yanlis toplam
	}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(part+".state", data, 0o644); err != nil {
		t.Fatal(err)
	}

	srv := rangeServer(t, `"v1"`, nil)
	d := &Downloader{Client: srv.Client()}
	_, err := d.Download(context.Background(), out, testItem(srv.URL+"/veri.bin", "veri.bin"))
	if err == nil {
		t.Fatal("bozuk .part icin hata bekleniyordu; 416 'tamamlandi' demek degil")
	}
	if _, serr := os.Stat(filepath.Join(out, "veri.bin")); serr == nil {
		t.Fatal("bozuk .part nihai adla yazildi")
	}
	if fi, serr := os.Stat(part); serr == nil && fi.Size() != 0 {
		t.Fatalf(".part sifirlanmadi: %d bayt", fi.Size())
	}
}

func TestChunkedResponseStillUsesPartFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Content-Length YOK: chunked. Resume denenmemeli ama .part kullanilmali.
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/veri.bin", "veri.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("chunked: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(out, "veri.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("chunked icerik bozuk")
	}
}

func TestDuplicateFilenameGetsDeterministicSuffix(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	// Farkli item'lar FARKLI SourcePage tasir; ad sahipligi kimlikle tutuluyor.
	a := testItem(srv.URL+"/veri.bin", "ayni.bin")
	a.Index = 0
	a.SourcePage = "https://ornek.test/u/a"
	b := testItem(srv.URL+"/veri.bin", "ayni.bin")
	b.Index = 4
	b.SourcePage = "https://ornek.test/u/b"

	if _, err := d.Download(context.Background(), out, a); err != nil {
		t.Fatalf("ilk: %v", err)
	}
	if _, err := d.Download(context.Background(), out, b); err != nil {
		t.Fatalf("ikinci: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "ayni.bin")); err != nil {
		t.Error("ilk dosya duz adla yazilmaliydi")
	}
	// Sonek Index'ten turer, boylece kosular arasinda deterministiktir.
	if _, err := os.Stat(filepath.Join(out, "ayni (5).bin")); err != nil {
		t.Errorf("ikinci dosya 'ayni (5).bin' olmaliydi: %v", err)
	}
}

func TestExpiredSignedURLTriggersReresolveOnce(t *testing.T) {
	good := rangeServer(t, `"v1"`, nil)
	expired := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(expired.Close)

	calls := 0
	out := tempDir(t)
	d := &Downloader{
		Client: good.Client(),
		Reresolve: func(ctx context.Context, sourcePage string) (site.Item, error) {
			calls++
			return testItem(good.URL+"/veri.bin", "veri.bin"), nil
		},
	}
	it := testItem(expired.URL+"/veri.bin", "veri.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Reresolve %d kez cagrildi, 1 bekleniyordu", calls)
	}
	got, _ := os.ReadFile(filepath.Join(out, "veri.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("yeniden cozumleme sonrasi icerik bozuk")
	}
}

func TestExpiredURLWithoutReresolverIsPermanent(t *testing.T) {
	expired := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	t.Cleanup(expired.Close)
	d := &Downloader{Client: expired.Client()}
	_, err := d.Download(context.Background(), tempDir(t), testItem(expired.URL+"/x", "veri.bin"))
	if err == nil {
		t.Fatal("Reresolve yokken 410 kalici hata olmali")
	}
}

// .part state'ten kisaysa resume dosyanin basina delik acar. Tek guvenli
// davranis bastan baslamak, ve sonucun BOZUK OLMAMASI.
func TestPartShorterThanStateRestartsCleanly(t *testing.T) {
	out := tempDir(t)
	part := filepath.Join(out, "veri.bin.part")
	if err := os.WriteFile(part, []byte("kisa"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := State{Offset: 9999, Validator: `"v1"`, ValidatorType: ValidatorETag, TotalSize: int64(len(payload))}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(part+".state", data, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := rangeServer(t, `"v1"`, nil)
	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/veri.bin", "veri.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("bastan indirme basarisiz: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(out, "veri.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("icerik bozuk")
	}
}

func TestParseContentRange(t *testing.T) {
	cases := []struct {
		in         string
		start, tot int64
		wantErr    bool
	}{
		{"bytes 100-199/1234", 100, 1234, false},
		{"bytes 0-0/1", 0, 1, false},
		{"bytes 500-999/*", 500, -1, false},
		{"sayfa 1-2/3", 0, 0, true},
		{"bytes bozuk", 0, 0, true},
		{"", 0, 0, true},
	}
	for _, c := range cases {
		s, tt, err := parseContentRange(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseContentRange(%q) hata bekleniyordu", c.in)
			}
			continue
		}
		if err != nil || s != c.start || tt != c.tot {
			t.Errorf("parseContentRange(%q) = %d,%d,%v", c.in, s, tt, err)
		}
	}
}

// --- Ad sahipligi ---

// AYNI item adi ikinci kez isterse ayni adi geri almali: duraklat/devam bu
// olmadan "(N)" ekli yeni bir ada sapar ve yarim .part oksuz kalir.
func TestClaimIsIdempotentForSameItem(t *testing.T) {
	d := &Downloader{}
	a := site.Item{SourcePage: "https://s.test/f/a", Filename: "video.mp4"}
	first := d.claim("out", a)
	second := d.claim("out", a)
	if first != "video.mp4" || second != first {
		t.Fatalf("ayni item icin adlar farkli: %q, %q", first, second)
	}
	// FARKLI bir item ayni adi isterse ek almali.
	b := site.Item{SourcePage: "https://s.test/f/b", Filename: "video.mp4", Index: 1}
	if got := d.claim("out", b); got == "video.mp4" {
		t.Fatalf("farkli item ayni adi aldi: %q", got)
	}
}

// Plan, Download'in kullanacagi yolu onceden ve tutarli soylemeli.
func TestPlanMatchesDownloadPath(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/veri.bin", "veri.bin")
	it.Dir = "Albüm"

	planned, err := d.Plan(out, it)
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != planned {
		t.Fatalf("Plan %q dedi, Download %q yazdi", planned, res.Path)
	}
}
