package dl

// Bu dosya, kod incelemesinde bulunan hatalar için regresyon testleri tutar.
// Her test bir bulguya karşılık gelir ve düzeltmenin geri alınmasını engeller.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// BULGU 1. En sinsi tutarsızlık: `.part` silinmiş ama `.part.state` duruyor ve
// kaydedilmiş sha256 durumunu taşıyor. Naif resume dosyanın başına offset kadar
// SIFIR deliği açar; hash durumu state'ten geldiği için sha256 kontrolü GEÇER
// ve bozuk dosya nihai adıyla yazılır.
//
// Düzeltmeden önce ölçüldü: 20000 sıfır baytlı dosya "OK" olarak yazıldı ve
// Download nil döndü.
func TestMissingPartWithStateMustNotCorrupt(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	part := filepath.Join(out, "veri.bin.part")

	h := sha256.New()
	h.Write(payload[:20000])
	hs, err := h.(interface{ MarshalBinary() ([]byte, error) }).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	st := State{
		Offset: 20000, Validator: `"v1"`, ValidatorType: ValidatorETag,
		TotalSize: int64(len(payload)), SHA256State: hs,
	}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(part+".state", data, 0o644); err != nil {
		t.Fatal(err)
	}
	// .part KASITLI olarak yok.

	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/veri.bin", "veri.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("baştan indirme başarısız olmamalı: %v", err)
	}
	got, rerr := os.ReadFile(filepath.Join(out, "veri.bin"))
	if rerr != nil {
		t.Fatalf("dosya yok: %v", rerr)
	}
	if !bytes.Equal(got, payload) {
		zeros := bytes.Count(got[:20000], []byte{0})
		t.Fatalf("BOZUK DOSYA: ilk 20000 baytta %d sıfır var", zeros)
	}
}

// BULGU 2. Hash uyuşmazlığında `.part` ve state diskte kalırsa her koşu aynı
// hatayı tekrarlar ve item elle silinmeden kurtarılamaz.
func TestSHA256MismatchCleansUpForRetry(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)

	bad := testItem(srv.URL+"/veri.bin", "veri.bin")
	bad.SHA256 = strings.Repeat("00", 32)
	d := &Downloader{Client: srv.Client()}
	if _, err := d.Download(context.Background(), out, bad); !errors.Is(err, ErrSHA256Mismatch) {
		t.Fatalf("ErrSHA256Mismatch bekleniyordu: %v", err)
	}
	part := filepath.Join(out, "veri.bin.part")
	for _, f := range []string{part, part + ".state"} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s temizlenmedi; sonraki koşu aynı hataya düşer", filepath.Base(f))
		}
	}

	// Doğru hash ile aynı klasörde tekrar denenebilmeli.
	good := testItem(srv.URL+"/veri.bin", "veri.bin")
	good.SHA256 = payloadSHA()
	d2 := &Downloader{Client: srv.Client()}
	if _, err := d2.Download(context.Background(), out, good); err != nil {
		t.Fatalf("temizlikten sonra tekrar denenemiyor: %v", err)
	}
}

// BULGU 3. user_agent ve Item.Headers transfer isteğine de uygulanmalı.
// Yalnızca API çağrılarına uygulanması sessiz bir yarım uygulama, ve bunkr'da
// item sayfası Referer'ı transferde zorunlu.
func TestUserAgentAndHeadersAppliedToTransfer(t *testing.T) {
	var gotUA, gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotReferer = r.Header.Get("Referer")
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	d := &Downloader{Client: srv.Client(), UserAgent: "siphon/test"}
	it := testItem(srv.URL+"/veri.bin", "veri.bin")
	it.Headers = map[string]string{"Referer": "https://ornek.test/u/abc"}
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), tempDir(t), it); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if gotUA != "siphon/test" {
		t.Errorf("User-Agent = %q, transfere uygulanmadı", gotUA)
	}
	if gotReferer != "https://ornek.test/u/abc" {
		t.Errorf("Referer = %q; Item.Headers transfere uygulanmadı", gotReferer)
	}
}

// BULGU 5. Sınıflandırıcı varsa 403 "imzalı URL süresi doldu" sayılmamalı ve
// yeniden çözümleme TETİKLENMEMELİ. Aksi halde araç rate limitliyken URL'i
// yeniden çözüp tekrar dener, yani limiti kendi eliyle derinleştirir.
func TestClassifierPreventsReresolveOn403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"value":"transfer_limit_exceeded","message":"limit"}`))
	}))
	t.Cleanup(srv.Close)

	sentinel := errors.New("rate limit")
	calls := 0
	d := &Downloader{
		Client:    srv.Client(),
		Classify:  func(resp *http.Response, body []byte) error { return sentinel },
		Reresolve: func(context.Context, string) (site.Item, error) { calls++; return site.Item{}, nil },
	}
	_, err := d.Download(context.Background(), tempDir(t), testItem(srv.URL+"/x", "veri.bin"))
	if !errors.Is(err, sentinel) {
		t.Fatalf("sınıflandırıcının hatası bekleniyordu, %v geldi", err)
	}
	if calls != 0 {
		t.Fatalf("Reresolve %d kez çağrıldı; rate limitliyken yeniden çözmek limiti derinleştirir", calls)
	}
}

// Sınıflandırıcı nil döndürdüğünde (tanınmayan gövde) eski davranış korunmalı.
func TestClassifierReturningNilFallsBackToExpired(t *testing.T) {
	good := rangeServer(t, `"v1"`, nil)
	expired := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(expired.Close)

	calls := 0
	d := &Downloader{
		Client:   good.Client(),
		Classify: func(resp *http.Response, body []byte) error { return nil },
		Reresolve: func(context.Context, string) (site.Item, error) {
			calls++
			return testItem(good.URL+"/veri.bin", "veri.bin"), nil
		},
	}
	it := testItem(expired.URL+"/x", "veri.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), tempDir(t), it); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Reresolve %d kez çağrıldı, 1 bekleniyordu", calls)
	}
}

// BULGU 9. Content-Length yok ve hash yok: kırpılmış gövde "tamamlandı"
// sayılmamalı. Resolver'ın bildirdiği boyut yedek olarak devreye girer.
func TestTruncatedBodyCaughtByItemSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Content-Length YOK ve gövde kasıtlı olarak kısa.
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload[:1000])
	}))
	t.Cleanup(srv.Close)

	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/veri.bin", "veri.bin") // Size = len(payload)
	if _, err := d.Download(context.Background(), out, it); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("ErrIncomplete bekleniyordu, %v geldi", err)
	}
	if _, serr := os.Stat(filepath.Join(out, "veri.bin")); serr == nil {
		t.Fatal("kırpılmış gövde nihai adla yazıldı")
	}
}

// Resolver boyutu bilmiyorsa (Size -1) kontrol yapılamaz; eski davranış korunur.
// Bu, chunked yanıtta resume denenmemesi kararının da dokunulmadığını gösterir.
func TestUnknownSizeChunkedStillCompletes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/veri.bin", "veri.bin")
	it.Size = -1
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), tempDir(t), it); err != nil {
		t.Fatalf("Download: %v", err)
	}
}

// BULGU 8. Üretilen "(N)" adının kendisi de çakışabilir; o durumda ikinci item
// "zaten var" dalına düşüp hiç indirilmeden OK raporlanırdı.
func TestGeneratedDedupNameIsAlsoDeduped(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	// Üçü de çakışıyor: b'nin üreteceği ad ("ayni (2).bin") c'nin düz adıyla aynı.
	a := testItem(srv.URL+"/veri.bin", "ayni.bin")
	a.Index = 0
	b := testItem(srv.URL+"/veri.bin", "ayni.bin")
	b.Index = 1
	c := testItem(srv.URL+"/veri.bin", "ayni (2).bin")
	c.Index = 2

	for i, it := range []site.Item{a, b, c} {
		if _, err := d.Download(context.Background(), out, it); err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("%d dosya oluştu, 3 bekleniyordu: %v", len(entries), names)
	}
}
