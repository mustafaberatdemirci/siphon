package run

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/dl"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/store"
)

// tempDir, Windows'ta t.TempDir() temizliğinin virüs tarayıcı yüzünden
// "Dizin boş değil" ile patlamasını engelliyor.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "siphon-run-")
	if err != nil {
		t.Fatalf("geçici dizin: %v", err)
	}
	t.Cleanup(func() {
		for i := 0; i < 10; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return dir
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
