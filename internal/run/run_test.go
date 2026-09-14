package run

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/dl"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/store"
	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

// tempDir, paylaşılan yardımcıya devrediyor (Windows dosya kilidi sorunu).
func tempDir(t *testing.T) string {
	t.Helper()
	return testutil.TempDir(t)
}

// fakeResolver, yield'i çağırıp verilen item'ları döndürür. Gerçek resolver
// yerine kullanılıyor çünkü test edilen şey run.go'nun OLAY SIRASI, site
// protokolü değil.
type fakeResolver struct {
	items []site.Item
}

func (f *fakeResolver) Match(string) bool { return true }

func (f *fakeResolver) Resolve(ctx context.Context, u string, yield func(site.Item) error) ([]site.ItemError, error) {
	for _, it := range f.items {
		if err := yield(it); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (f *fakeResolver) ResolveOne(context.Context, string) (site.Item, error) {
	return site.Item{}, fmt.Errorf("beklenmeyen yeniden çözümleme")
}

func (f *fakeResolver) Diagnose(context.Context) ([]site.LayerResult, error) { return nil, nil }

// newRunCtx, runOne için hazır bir koşu bağlamı kurar: gerçek indirici,
// gerçek kayıt, sahte resolver.
func newRunCtx(t *testing.T, outDir string, r site.Resolver, ev Events) runCtx {
	t.Helper()
	ledger, err := store.Open(outDir)
	if err != nil {
		t.Fatalf("kayıt açılamadı: %v", err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	return runCtx{
		url:      "https://fake.test/a/1",
		resolver: r,
		cfg:      site.SiteConfig{Name: "fake"}.WithDefaults(),
		client:   http.DefaultClient,
		ledger:   ledger,
		opt:      Options{OutDir: outDir},
		ev:       ev,
		inFlight: 4,
	}
}

func fakeItems(base string, n int) []site.Item {
	items := make([]site.Item, n)
	for i := range items {
		items[i] = site.Item{
			URL:        fmt.Sprintf("%s/f/%d", base, i),
			SourcePage: fmt.Sprintf("%s/p/%d", base, i),
			Filename:   fmt.Sprintf("d%d.bin", i),
			Size:       -1,
			Index:      i,
		}
	}
	return items
}

// ASIL HATA BUYDU: arayüz toplam dosya sayısını URLResolved'dan sayıyordu, o
// olay ise URL'in TÜM item'ları bitince tetikleniyor. Sonuçta ilerleme çubuğu
// koşu boyunca sıfırda kalıyordu.
//
// Bu test sözleşmeyi kaynağından doğruluyor: TÜM ItemQueued olayları, HİÇBİR
// dosya bitmemişken gelmiş olmalı. Sunucu gövdeyi serbest bırakılana kadar
// tutuyor, böylece sonuç zamanlamaya bağlı kalmıyor.
func TestItemQueuedArrivesBeforeAnyDownloadFinishes(t *testing.T) {
	outDir := tempDir(t)

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
		_, _ = w.Write([]byte("veri"))
	}))
	defer srv.Close()

	const n = 3
	var (
		mu          sync.Mutex
		queued      int
		finished    int
		doneAtQueue []int // her ItemQueued anında kaç dosya bitmişti
	)
	allQueued := make(chan struct{})

	ev := Events{
		ItemQueued: func(site.Item) {
			mu.Lock()
			queued++
			doneAtQueue = append(doneAtQueue, finished)
			q := queued
			mu.Unlock()
			if q == n {
				close(allQueued)
			}
		},
		ItemDone: func(site.Item, dl.Result) {
			mu.Lock()
			finished++
			mu.Unlock()
		},
	}

	rc := newRunCtx(t, outDir, &fakeResolver{items: fakeItems(srv.URL, n)}, ev)

	go func() {
		// Tüm item'lar kuyruğa girdikten SONRA indirmelerin bitmesine izin ver.
		select {
		case <-allQueued:
		case <-time.After(10 * time.Second):
			// Kilitlenmeyi test zaman aşımına bırakmamak için yine de bırak.
		}
		close(release)
	}()

	res := runOne(context.Background(), rc)

	if res.resolveErr != nil {
		t.Fatalf("çözümleme hatası: %v", res.resolveErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if queued != n {
		t.Fatalf("ItemQueued %d kez geldi, %d bekleniyordu", queued, n)
	}
	for i, d := range doneAtQueue {
		if d != 0 {
			t.Errorf("%d. ItemQueued geldiğinde %d dosya bitmişti; kuyruk bildirimi iş bitmeden gelmeliydi", i+1, d)
		}
	}
	if res.done != n {
		t.Fatalf("inen dosya = %d, %d bekleniyordu", res.done, n)
	}
}

// quotaResolver, 509'u kota olarak sınıflar (mega gibi).
type quotaResolver struct{ fakeResolver }

func (quotaResolver) ClassifyStatus(resp *http.Response, _ []byte) error {
	if resp.StatusCode == 509 {
		return &site.QuotaError{Wait: time.Hour, Err: site.Errorf(site.LayerCDN, "fake", "kota doldu (sahte)")}
	}
	return nil
}

// Kota dolunca albüm DURMALI: 373 dosyalık bir mega klasöründe kalan her
// item sırayla aynı 509'u alır, hiçbiri inmez, yalnızca istek harcanır.
// Sunucu kaç istek gördüğünü sayıyor; 509 sonrası yeni item başlamamalı.
func TestQuotaHaltsTheAlbum(t *testing.T) {
	outDir := tempDir(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "Bandwidth Limit Exceeded", 509)
	}))
	defer srv.Close()

	const n = 40
	rc := newRunCtx(t, outDir, &quotaResolver{fakeResolver{items: fakeItems(srv.URL, n)}}, Events{})
	rc.inFlight = 1 // sıralı: ilk 509'dan sonra kaç item daha denendiği net ölçülsün
	res := runOne(context.Background(), rc)

	if !res.halted {
		t.Fatal("kota albümü durdurmadı")
	}
	if h := hits.Load(); h > 2 {
		t.Errorf("509 sonrası %d istek daha atıldı; albüm ilk kotada durmalıydı", h-1)
	}
	if res.failed != 1 {
		t.Errorf("failed = %d, 1 bekleniyordu (kota alan item)", res.failed)
	}
}

// quotaProbeResolver: quotaResolver + QuotaAvailable (mega gibi).
type quotaProbeResolver struct {
	quotaResolver
	avail atomic.Bool
}

func (r *quotaProbeResolver) QuotaAvailable(context.Context) (bool, error) {
	return r.avail.Load(), nil
}

// -on-quota: kota dolunca komut çalışır, site pay verince AYNI URL yeniden
// koşulur ve kayıt inenleri atlar. Sunucu ilk N isteği karşılıyor, sonra 509
// veriyor, komut çalışınca (işaret dosyası) tekrar açılıyor.
func TestOnQuotaCommandThenRetriesSameURL(t *testing.T) {
	outDir := tempDir(t)
	marker := filepath.Join(tempDir(t), "vpn.txt")
	var served atomic.Int32
	quotaOn := atomic.Bool{}
	quotaOn.Store(false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(marker); err == nil {
			quotaOn.Store(false) // "VPN değişti"
		}
		if served.Load() >= 2 && !quotaOn.Load() {
			// 2 dosya indi; komut çalışmadıysa kota dolsun.
			if _, err := os.Stat(marker); err != nil {
				quotaOn.Store(true)
			}
		}
		if quotaOn.Load() {
			http.Error(w, "Bandwidth Limit Exceeded", 509)
			return
		}
		served.Add(1)
		w.Header().Set("Content-Length", "4")
		_, _ = w.Write([]byte("veri"))
	}))
	defer srv.Close()

	const n = 5
	r := &quotaProbeResolver{quotaResolver: quotaResolver{fakeResolver{items: fakeItems(srv.URL, n)}}}
	rc := newRunCtx(t, outDir, r, Events{})
	rc.inFlight = 1
	rc.opt.OnQuota = `echo degisti> "` + marker + `"`
	rc.opt.QuotaProbeEvery = 50 * time.Millisecond
	rc.opt.QuotaProbeMax = 5 * time.Second

	// Yoklama komut bittikten sonra "var" desin: komutun ürettiği dosyaya bak.
	go func() {
		for {
			if _, err := os.Stat(marker); err == nil {
				r.avail.Store(true)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	var sum Summary
	res := runURL(context.Background(), rc, &sum)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("kota komutu çalışmadı")
	}
	if sum.Done != n {
		t.Fatalf("toplam inen %d, %d bekleniyordu (ikinci tur kalanları indirmeli)", sum.Done, n)
	}
	if sum.Skipped != 2 {
		t.Errorf("ikinci turda kayıt %d dosyayı atladı, 2 bekleniyordu", sum.Skipped)
	}
	if res.quota {
		t.Error("son tur yine kotada bitti")
	}
}

// Kuyruğa giren her item MUTLAKA Done, Failed veya Skipped ile kapanmalı;
// aksi halde ilerleme çubuğu yüzde yüze hiç ulaşmaz.
func TestEveryQueuedItemIsAccountedFor(t *testing.T) {
	outDir := tempDir(t)

	// Üçüncü dosya 404 veriyor: karışık sonuçta da muhasebe tutmalı.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/f/2" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("veri"))
	}))
	defer srv.Close()

	const n = 4
	var mu sync.Mutex
	counts := map[string]int{}
	ev := Events{
		ItemQueued:  func(site.Item) { mu.Lock(); counts["queued"]++; mu.Unlock() },
		ItemDone:    func(site.Item, dl.Result) { mu.Lock(); counts["done"]++; mu.Unlock() },
		ItemFailed:  func(site.Item, error) { mu.Lock(); counts["failed"]++; mu.Unlock() },
		ItemSkipped: func(site.Item, store.Entry) { mu.Lock(); counts["skipped"]++; mu.Unlock() },
	}

	rc := newRunCtx(t, outDir, &fakeResolver{items: fakeItems(srv.URL, n)}, ev)
	rc.cfg.MaxRetries = 1 // 404'te tekrar tekrar denemeyi bekleme
	_ = runOne(context.Background(), rc)

	mu.Lock()
	defer mu.Unlock()
	if counts["queued"] != n {
		t.Fatalf("kuyruk = %d, %d bekleniyordu", counts["queued"], n)
	}
	closed := counts["done"] + counts["failed"] + counts["skipped"]
	if closed != n {
		t.Fatalf("kapanan item = %d (done=%d failed=%d skipped=%d), %d bekleniyordu: "+
			"kapanmayan item ilerleme çubuğunu doldurmaz",
			closed, counts["done"], counts["failed"], counts["skipped"], n)
	}
	if counts["failed"] == 0 {
		t.Error("404 veren dosya başarısız sayılmadı")
	}
}

// İkinci koşuda dosyalar zaten kayıtlı: her item yine kuyruğa girmeli ve
// Skipped ile kapanmalı. Aksi halde "hepsi zaten inmiş" durumunda çubuk
// boşta kalır.
func TestSecondRunQueuesAndSkipsEveryItem(t *testing.T) {
	outDir := tempDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("veri"))
	}))
	defer srv.Close()

	const n = 3
	items := fakeItems(srv.URL, n)

	first := runOne(context.Background(), newRunCtx(t, outDir, &fakeResolver{items: items}, Events{}))
	if first.done != n {
		t.Fatalf("ilk koşuda inen = %d, %d bekleniyordu", first.done, n)
	}
	for i := 0; i < n; i++ {
		if _, err := os.Stat(filepath.Join(outDir, fmt.Sprintf("d%d.bin", i))); err != nil {
			t.Fatalf("dosya yok: %v", err)
		}
	}

	var mu sync.Mutex
	var queued, skipped int
	ev := Events{
		ItemQueued:  func(site.Item) { mu.Lock(); queued++; mu.Unlock() },
		ItemSkipped: func(site.Item, store.Entry) { mu.Lock(); skipped++; mu.Unlock() },
	}
	second := runOne(context.Background(), newRunCtx(t, outDir, &fakeResolver{items: items}, ev))

	mu.Lock()
	defer mu.Unlock()
	if queued != n {
		t.Fatalf("ikinci koşuda kuyruk = %d, %d bekleniyordu", queued, n)
	}
	if skipped != n {
		t.Fatalf("atlanan = %d, %d bekleniyordu", skipped, n)
	}
	if second.skipped != n {
		t.Fatalf("özet skipped = %d, %d bekleniyordu", second.skipped, n)
	}
}
