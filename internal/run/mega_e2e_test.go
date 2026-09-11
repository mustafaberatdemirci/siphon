package run

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/dl"
	"github.com/mustafaberatdemirci/siphon/internal/megacrypto"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/store"
)

// Uctan uca: gercek mega cozumleyici + gercek indirici + gercek kayit, tek bir
// httptest sunucusu hem API hem depolama rolunde. Sifreli govde tel uzerinden
// geliyor, diske DUZ METIN yaziliyor ve meta-MAC dogrulaniyor.
//
// mega_test.go'daki birim testler cozumleyiciyi tek basina kanitliyor; bu test
// parcalarin BIRLIKTE calistigini: Item.Secret'in indiriciye ulastigini,
// cozucunun dogru offset'le kuruldugunu, resume'da cozucu durumunun state
// dosyasindan geri geldigini.

type megaE2E struct {
	t      *testing.T
	srv    *httptest.Server
	plain  []byte
	packed []byte
	enc    []byte
	name   string

	mu        sync.Mutex
	hold      chan struct{} // nil degilse ILK govde bu kanala kadar bekler
	holdUsed  bool          // kanal referansi test'te kaliyor; tuketildi mi ayri izleniyor
	firstOnly int           // hold varken ilk yanitta verilecek bayt
	dlHits    int
}

func newMegaE2E(t *testing.T, size int) *megaE2E {
	t.Helper()
	e := &megaE2E{t: t, name: "Gizli Video — Özgür.mp4"}
	e.plain = make([]byte, size)
	if _, err := rand.Read(e.plain); err != nil {
		t.Fatal(err)
	}
	aesKey, nonce := make([]byte, 16), make([]byte, 8)
	_, _ = rand.Read(aesKey)
	_, _ = rand.Read(nonce)
	mac, err := megacrypto.MetaMACOf(aesKey, nonce, e.plain)
	if err != nil {
		t.Fatal(err)
	}
	e.packed = megacrypto.PackFileKey(aesKey, nonce, mac)
	e.enc, err = megacrypto.EncryptCTR(aesKey, nonce, e.plain)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/cs", e.api)
	mux.HandleFunc("/dl/", e.download)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *megaE2E) api(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var cmds []map[string]any
	if err := json.Unmarshal(body, &cmds); err != nil || len(cmds) == 0 {
		fmt.Fprint(w, "-2")
		return
	}
	if cmds[0]["a"] != "g" {
		fmt.Fprint(w, "[-2]")
		return
	}
	key, _ := megacrypto.UnpackFileKey(e.packed)
	at, _ := megacrypto.EncryptAttrs(key.AES, megacrypto.Attrs{Name: e.name})
	_ = json.NewEncoder(w).Encode([]any{map[string]any{
		"s": len(e.plain), "at": at, "g": e.srv.URL + "/dl/FiLeHaNd",
	}})
}

func (e *megaE2E) download(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.dlHits++
	var hold chan struct{}
	if e.hold != nil && !e.holdUsed {
		hold, e.holdUsed = e.hold, true
	}
	first := e.firstOnly
	e.mu.Unlock()

	w.Header().Set("ETag", `"mega-v1"`)
	if hold != nil {
		// Ilk govde: parcayi ver, sonra sonsuza kadar bekle (kesinti simulasyonu).
		w.Header().Set("Content-Length", fmt.Sprint(len(e.enc)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(e.enc[:first])
		w.(http.Flusher).Flush()
		<-hold
		return
	}
	http.ServeContent(w, r, "x.bin", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), bytes.NewReader(e.enc))
}

func (e *megaE2E) link() string {
	return "https://mega.nz/file/FiLeHaNd#" + megacrypto.B64Encode(e.packed)
}

func (e *megaE2E) resolver() site.Resolver {
	cfg := site.SiteConfig{
		Name:          site.MegaName,
		Domains:       []string{"mega.nz"},
		RefererPolicy: site.RefererNone,
		Extra:         map[string]string{site.ExtraMegaAPI: e.srv.URL + "/cs"},
		HTTPClient:    e.srv.Client(),
	}.WithDefaults()
	return site.NewMega(cfg)
}

func (e *megaE2E) runCtx(t *testing.T, outDir string, ev Events, ctx context.Context) runCtx {
	t.Helper()
	ledger, err := store.Open(outDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	r := e.resolver()
	cfg := site.SiteConfig{Name: site.MegaName}.WithDefaults()
	cfg.MaxRetries = 2
	return runCtx{
		url: e.link(), resolver: r, cfg: cfg, client: e.srv.Client(),
		ledger: ledger, opt: Options{OutDir: outDir}, ev: ev, inFlight: 2,
	}
}

func TestMegaEndToEndDownloadsPlaintext(t *testing.T) {
	e := newMegaE2E(t, 300*1024+11)
	out := tempDir(t)

	var done []dl.Result
	var failed []error
	ev := Events{
		ItemDone:   func(_ site.Item, r dl.Result) { done = append(done, r) },
		ItemFailed: func(_ site.Item, err error) { failed = append(failed, err) },
	}
	res := runOne(context.Background(), e.runCtx(t, out, ev, context.Background()))
	if res.resolveErr != nil {
		t.Fatalf("cozumleme: %v", res.resolveErr)
	}
	if len(failed) != 0 {
		t.Fatalf("basarisiz: %v", failed)
	}
	if len(done) != 1 {
		t.Fatalf("%d dosya indi, 1 bekleniyordu", len(done))
	}

	// Diske yazilan sey DUZ METIN olmali, tel uzerinden gelen sifreli govde degil.
	got, err := os.ReadFile(done[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, e.plain) {
		if bytes.Equal(got, e.enc) {
			t.Fatal("diske SIFRELI govde yazildi: cozucu devreye girmemis")
		}
		t.Fatal("icerik ne duz metin ne sifreli: bozuk")
	}
	if filepath.Base(done[0].Path) != e.name {
		t.Errorf("dosya adi = %q, oznitelikten gelen ad bekleniyordu", filepath.Base(done[0].Path))
	}
	want := sha256.Sum256(e.plain)
	if done[0].SHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("kaydedilen sha256 duz metnin degil")
	}

	// Kayit, kaynak linki oldugu gibi saklar ve mega'da link anahtari TASIR
	// (# sonrasi). Bu kasitli: yeniden cozumleme o anahtara muhtac ve link
	// zaten kullanicinin elinde olan sey. Burada yalnizca kaydin yazildigi
	// ve dosyaya isaret ettigi dogrulaniyor.
	ledgerBytes, _ := os.ReadFile(filepath.Join(out, store.FileName))
	if !strings.Contains(string(ledgerBytes), e.name) {
		t.Error("kayit dosyasi indirilen dosyayi icermiyor")
	}
}

// Kesinti + resume, GERCEK indirici uzerinden: cozucu durumu state dosyasina
// yazilmali ve ikinci kosu oradan devam edip meta-MAC'i gecmeli.
func TestMegaEndToEndResume(t *testing.T) {
	e := newMegaE2E(t, 400*1024+3)
	out := tempDir(t)

	// 1. kosu: 150 KB verip kilitlenen sunucu, context iptaliyle kesilir.
	e.hold = make(chan struct{})
	e.firstOnly = 150 * 1024
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	rc := e.runCtx(t, out, Events{}, ctx)
	_ = runOne(ctx, rc)
	close(e.hold) // tutulan handler'i serbest birak; sunucu Cleanup'ta onu bekliyor

	part := filepath.Join(out, e.name+".part")
	statePath := part + ".state"
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state yok, kesinti kaydedilmemis: %v", err)
	}
	var st dl.State
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Offset <= 0 || st.Offset >= int64(len(e.plain)) {
		t.Fatalf("offset = %d, kismi ilerleme bekleniyordu", st.Offset)
	}
	if len(st.DecoderState) == 0 {
		t.Fatal("state'te cozucu durumu yok; resume MAC'i yanlis hesaplar")
	}

	// 2. kosu: normal sunucu, kaldigi yerden.
	var done []dl.Result
	var failed []error
	ev := Events{
		ItemDone:   func(_ site.Item, r dl.Result) { done = append(done, r) },
		ItemFailed: func(_ site.Item, err error) { failed = append(failed, err) },
	}
	res := runOne(context.Background(), e.runCtx(t, out, ev, context.Background()))
	if res.resolveErr != nil || len(failed) != 0 {
		t.Fatalf("resume kosusu: %v / %v", res.resolveErr, failed)
	}
	if len(done) != 1 {
		t.Fatalf("%d dosya indi, 1 bekleniyordu", len(done))
	}
	got, _ := os.ReadFile(done[0].Path)
	if !bytes.Equal(got, e.plain) {
		t.Fatal("resume sonrasi icerik bozuk")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Error("tamamlanan dosyanin state'i silinmedi")
	}
	// Ikinci kosu Range ile devam etmis olmali, bastan indirmemis.
	e.mu.Lock()
	hits := e.dlHits
	e.mu.Unlock()
	if hits != 2 {
		t.Errorf("depolama %d kez cagrildi, 2 bekleniyordu (1 kesik + 1 resume)", hits)
	}
}

// Ucuncu kosu: dosya zaten kayitli -> atlanmali, API'ye bile gidilmemeli.
func TestMegaEndToEndSecondRunSkips(t *testing.T) {
	e := newMegaE2E(t, 64*1024)
	out := tempDir(t)

	first := runOne(context.Background(), e.runCtx(t, out, Events{}, context.Background()))
	if first.done != 1 {
		t.Fatalf("ilk kosu: %+v", first)
	}
	var skipped int
	ev := Events{ItemSkipped: func(site.Item, store.Entry) { skipped++ }}
	second := runOne(context.Background(), e.runCtx(t, out, ev, context.Background()))
	if second.skipped != 1 || skipped != 1 {
		t.Fatalf("ikinci kosu atlamadi: %+v", second)
	}
}
