package dl

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// --- Parcali testler icin sunucu ---
//
// Buyuk bir payload (parcalama esigi 8 MiB; testte esik dusuruluyor), Range
// destegi, es zamanli baglanti sayaci ve istege bagli "Range'i yok say" kipi.

type segServer struct {
	t       *testing.T
	srv     *httptest.Server
	body    []byte
	etag    string
	ignore  bool  // Range'i yok say, hep 200 don
	hits    int32 // toplam istek
	ranges  []string
	mu      sync.Mutex
	inFly   int32
	maxFly  int32
	holdAll chan struct{} // nil degilse govdeler ilk 4 KB'den sonra bekler
	limit   int32         // >0 ise bu kadardan fazla es zamanli istek 503 alir
	rejects int32
}

func newSegServer(t *testing.T, size int) *segServer {
	t.Helper()
	s := &segServer{t: t, etag: `"seg-v1"`}
	s.body = make([]byte, size)
	if _, err := rand.Read(s.body); err != nil {
		t.Fatal(err)
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *segServer) serve(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&s.hits, 1)
	n := atomic.AddInt32(&s.inFly, 1)
	defer atomic.AddInt32(&s.inFly, -1)
	for {
		m := atomic.LoadInt32(&s.maxFly)
		if n <= m || atomic.CompareAndSwapInt32(&s.maxFly, m, n) {
			break
		}
	}
	s.mu.Lock()
	s.ranges = append(s.ranges, r.Header.Get("Range"))
	hold := s.holdAll
	s.mu.Unlock()

	if lim := atomic.LoadInt32(&s.limit); lim > 0 && n > lim {
		atomic.AddInt32(&s.rejects, 1)
		http.Error(w, "cok fazla baglanti", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("ETag", s.etag)
	if s.ignore {
		w.Header().Set("Content-Length", fmt.Sprint(len(s.body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(s.body)
		return
	}
	if hold != nil && r.Header.Get("Range") != "bytes=0-0" {
		// Aralik istegini kismen karsila, sonra bekle.
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(s.body)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		chunk := int64(4096)
		if end-start+1 < chunk {
			chunk = end - start + 1
		}
		_, _ = w.Write(s.body[start : start+chunk])
		w.(http.Flusher).Flush()
		<-hold
		return
	}
	http.ServeContent(w, r, "buyuk.bin", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), bytes.NewReader(s.body))
}

func (s *segServer) rangeHeaders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ranges...)
}

func (s *segServer) sha() string {
	h := sha256.Sum256(s.body)
	return hex.EncodeToString(h[:])
}

func segItem(s *segServer, name string) site.Item {
	return site.Item{URL: s.srv.URL + "/buyuk.bin", SourcePage: "https://ornek.test/u/seg", Filename: name, Size: int64(len(s.body))}
}

// --- Testler ---

func TestPlanSegments(t *testing.T) {
	segs := planSegments(100, 3)
	if len(segs) != 3 || segs[0].Start != 0 || segs[0].End != 33 || segs[2].Start != 66 || segs[2].End != 100 {
		t.Fatalf("plan yanlis: %+v", segs)
	}
	// Bosluk ve ortusme YOK.
	var covered int64
	for i, sg := range segs {
		covered += sg.End - sg.Start
		if i > 0 && sg.Start != segs[i-1].End {
			t.Fatalf("parcalar bitisik degil: %+v", segs)
		}
	}
	if covered != 100 {
		t.Fatalf("kapsam %d, 100 bekleniyordu", covered)
	}
	if got := planSegments(3, 8); len(got) != 3 {
		t.Errorf("boyuttan cok parca istendi: %d", len(got))
	}
	if got := planSegments(0, 4); len(got) != 1 {
		t.Errorf("sifir boyut: %+v", got)
	}
}

// Dort parca ile inen dosya bayt bayt dogru olmali, sha256 gecmeli ve
// sunucu gercekten dort ayri aralik gormeli.
func TestSegmentedDownloadIsCorrect(t *testing.T) {
	s := newSegServer(t, 1<<20)
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10}
	it := segItem(s, "buyuk.bin")
	it.SHA256 = s.sha()

	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, s.body) {
		t.Fatal("parcali indirme icerigi bozuk")
	}
	if res.SHA256 != s.sha() {
		t.Errorf("sha256 = %s", res.SHA256)
	}
	// 1 sonda + 4 parca; parca aralikları farkli olmali.
	ranges := map[string]bool{}
	for _, r := range s.rangeHeaders() {
		if r != "bytes=0-0" {
			ranges[r] = true
		}
	}
	if len(ranges) != 4 {
		t.Errorf("%d farkli aralik istendi, 4 bekleniyordu: %v", len(ranges), s.rangeHeaders())
	}
	if m := atomic.LoadInt32(&s.maxFly); m < 2 {
		t.Errorf("es zamanli baglanti en fazla %d oldu; paralellik yok", m)
	}
	if _, err := os.Stat(res.Path + ".part.state"); !os.IsNotExist(err) {
		t.Error("state dosyasi kaldi")
	}
}

// Kucuk dosya bolunmemeli: tek istek (sonda bile yok).
func TestSmallFileIsNotSegmented(t *testing.T) {
	s := newSegServer(t, 100<<10)
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), Segments: 4} // esik varsayilan 8 MiB
	if _, err := d.Download(context.Background(), out, segItem(s, "kucuk.bin")); err != nil {
		t.Fatal(err)
	}
	if h := atomic.LoadInt32(&s.hits); h != 1 {
		t.Errorf("kucuk dosya %d istekle indi, 1 bekleniyordu", h)
	}
}

// Sunucu Range'i yok sayarsa tek akisa dusulmeli ve dosya yine dogru inmeli.
func TestSegmentedFallsBackWhenRangeIgnored(t *testing.T) {
	s := newSegServer(t, 512<<10)
	s.ignore = true
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10}
	res, err := d.Download(context.Background(), out, segItem(s, "duz.bin"))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, s.body) {
		t.Fatal("tek akisa dusen indirme bozuk")
	}
}

// Cozucu varken (mega) parcalama KAPALI kalmali.
func TestDecoderDisablesSegments(t *testing.T) {
	d := &Downloader{Segments: 8, Decode: func(site.Item, int64, []byte, io.Reader) (site.DecodedStream, error) { return nil, nil }}
	if n := d.segmentsFor(site.Item{Size: 1 << 30}); n != 1 {
		t.Fatalf("cozucu varken %d parca", n)
	}
}

// Kesinti sonrasi devam: parca durumu diskte olmali ve ikinci kosu yalnizca
// eksik kalanlari istemeli (bastan indirmemeli).
func TestSegmentedResumeContinuesPartialSegments(t *testing.T) {
	s := newSegServer(t, 1<<20)
	s.mu.Lock()
	s.holdAll = make(chan struct{})
	s.mu.Unlock()
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10}
	it := segItem(s, "buyuk.bin")
	it.SHA256 = s.sha()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(600 * time.Millisecond); cancel() }()
	_, err := d.Download(ctx, out, it)
	s.mu.Lock()
	close(s.holdAll)
	s.holdAll = nil
	s.mu.Unlock()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("iptal bekleniyordu: %v", err)
	}

	statePath := filepath.Join(out, "buyuk.bin.part.state")
	raw, rerr := os.ReadFile(statePath)
	if rerr != nil {
		t.Fatalf("parca durumu diske yazilmamis: %v", rerr)
	}
	var st State
	_ = json.Unmarshal(raw, &st)
	if len(st.Segments) != 4 {
		t.Fatalf("state'te %d parca, 4 bekleniyordu", len(st.Segments))
	}
	var partial int64
	for _, sg := range st.Segments {
		partial += sg.Done
	}
	if partial == 0 {
		t.Fatal("hicbir parca ilerleme kaydetmemis")
	}
	if fi, _ := os.Stat(filepath.Join(out, "buyuk.bin.part")); fi == nil || fi.Size() != int64(len(s.body)) {
		t.Fatal(".part onceden boyutlandirilmamis")
	}

	before := len(s.rangeHeaders())
	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatalf("devam: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, s.body) {
		t.Fatal("devam sonrasi icerik bozuk")
	}
	// Devam istekleri parca baslangicindan DEGIL, kaldigi yerden olmali.
	for _, r := range s.rangeHeaders()[before:] {
		var a, b int64
		fmt.Sscanf(r, "bytes=%d-%d", &a, &b)
		for _, sg := range st.Segments {
			if a == sg.Start && sg.Done > 0 {
				t.Errorf("parca %d-%d bastan istendi; %d bayt zaten inmisti", sg.Start, sg.End, sg.Done)
			}
		}
	}
}

// Sunucu devam sirasinda dosyayi degistirirse (If-Range reddi -> 200) eski
// parcalar atilip bastan baslanmali; sonuc yine dogru olmali.
func TestSegmentedRestartsWhenSourceChanged(t *testing.T) {
	s := newSegServer(t, 512<<10)
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), Segments: 2, MinSegmentSize: 64 << 10}
	it := segItem(s, "degisen.bin")

	// Eski surumden kalma bir parcali durum uydur: dogrulayici farkli.
	part := filepath.Join(out, "degisen.bin.part")
	if err := os.WriteFile(part, make([]byte, len(s.body)), 0o644); err != nil {
		t.Fatal(err)
	}
	st := freshState()
	st.TotalSize = int64(len(s.body))
	st.Validator, st.ValidatorType = `"eski"`, ValidatorETag
	st.Segments = planSegments(st.TotalSize, 2)
	st.Segments[0].Done = 1000
	saveState(part+".state", st, nil)

	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		// Politika olmadan ilk deneme Retryable doner; ikinci Download temiz baslar.
		res, err = d.Download(context.Background(), out, it)
	}
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, s.body) {
		t.Fatal("kaynak degisince icerik bozuk kaldi")
	}
}

// Ek baglanti yuvalari host sinirina tabi: yuva yoksa parca sayisi duser.
func TestSegmentsRespectHostSlots(t *testing.T) {
	s := newSegServer(t, 1<<20)
	out := tempDir(t)
	var asked, granted int
	d := &Downloader{
		Client: s.srv.Client(), Segments: 8, MinSegmentSize: 64 << 10,
		AcquireExtra: func(_ string, want int) (int, func()) {
			asked, granted = want, 1 // yalnizca 1 ek yuva
			return 1, func() {}
		},
	}
	if _, err := d.Download(context.Background(), out, segItem(s, "sinirli.bin")); err != nil {
		t.Fatal(err)
	}
	if asked != 7 || granted != 1 {
		t.Fatalf("istenen %d, verilen %d", asked, granted)
	}
	if m := atomic.LoadInt32(&s.maxFly); m > 2 {
		t.Errorf("es zamanli %d baglanti; en fazla 2 olmaliydi (1 + 1 ek)", m)
	}
}

// sha256 uyusmazliginda .part silinmeli ve hata yeniden denenebilir OLMAMALI.
func TestSegmentedSHA256MismatchRemovesPart(t *testing.T) {
	s := newSegServer(t, 256<<10)
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), Segments: 2, MinSegmentSize: 64 << 10}
	it := segItem(s, "yanlis.bin")
	it.SHA256 = "00000000000000000000000000000000ffffffffffffffffffffffffffffffff"
	_, err := d.Download(context.Background(), out, it)
	if !errors.Is(err, ErrSHA256Mismatch) {
		t.Fatalf("ErrSHA256Mismatch bekleniyordu: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(out, "yanlis.bin.part")); !os.IsNotExist(serr) {
		t.Error(".part diskte kaldi")
	}
}

// Tek akisla yarim kalmis bir .part varsa parcaliya GECILMEMELI; tek akisla
// kaldigi yerden bitmeli.
func TestLinearPartIsNotConvertedToSegments(t *testing.T) {
	release := make(chan struct{})
	slow := slowServer(t, 20000, release)
	out := tempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	_, _ = (&Downloader{Client: slow.Client()}).Download(ctx, out, testItem(slow.URL+"/veri.bin", "veri.bin"))
	close(release)

	srv := rangeServer(t, `"v1"`, nil)
	d := &Downloader{Client: srv.Client(), Segments: 4, MinSegmentSize: 1024}
	it := testItem(srv.URL+"/veri.bin", "veri.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("tek akisla devam: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(out, "veri.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("icerik bozuk")
	}
}

// OLCULDU (bunkr CDN): ayni dosyaya 4 paralel aralik isteginden biri 503
// aliyor. Bir parcanin 503'u tum indirmeyi dusurmemeli; paralellik dusup
// indirme yine de bitmeli ve dogru olmali.
func TestSegmentedAdaptsToServerConnectionLimit(t *testing.T) {
	s := newSegServer(t, 1<<20)
	atomic.StoreInt32(&s.limit, 2)
	out := tempDir(t)
	// Logf birden fazla parca goroutine'inden gelir; kilitsiz append yaris.
	var logMu sync.Mutex
	var logs []string
	d := &Downloader{
		Client: s.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10,
		Logf: func(f string, a ...any) {
			logMu.Lock()
			logs = append(logs, fmt.Sprintf(f, a...))
			logMu.Unlock()
		},
	}
	it := segItem(s, "sinirli.bin")
	it.SHA256 = s.sha()

	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatalf("Download: %v (sunucu %d istegi reddetti)", err, atomic.LoadInt32(&s.rejects))
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, s.body) {
		t.Fatal("icerik bozuk")
	}
	if atomic.LoadInt32(&s.rejects) == 0 {
		t.Fatal("test kosulu olusmadi: sunucu hic 503 vermedi")
	}
	shrunk := false
	logMu.Lock()
	defer logMu.Unlock()
	for _, l := range logs {
		if strings.Contains(l, "paralellik") {
			shrunk = true
		}
	}
	if !shrunk {
		t.Errorf("503 sonrasi paralellik dusurulmedi; loglar: %v", logs)
	}
}
