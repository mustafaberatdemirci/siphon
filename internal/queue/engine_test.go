package queue

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

// --- Sahte site ---
//
// Tek httptest sunucusu N dosya sunuyor; her dosya Range destekli. "hold" ile
// bir dosyanin govdesi ilk parcadan sonra askiya alinabiliyor (duraklatma ve
// kapanis testleri icin).

type fakeSite struct {
	t     *testing.T
	srv   *httptest.Server
	files map[string][]byte // ad -> icerik

	mu    sync.Mutex
	hold  map[string]chan struct{} // ad -> serbest birakma kanali
	hits  map[string]int
	delay time.Duration
}

func newFakeSite(t *testing.T, names ...string) *fakeSite {
	t.Helper()
	f := &fakeSite{t: t, files: map[string][]byte{}, hold: map[string]chan struct{}{}, hits: map[string]int{}}
	for _, n := range names {
		b := make([]byte, 96*1024)
		_, _ = rand.Read(b)
		f.files[n] = b
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.mu.Lock()
		for _, ch := range f.hold {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
		f.mu.Unlock()
		f.srv.Close()
	})
	return f
}

func (f *fakeSite) serve(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/f/")
	body, ok := f.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	f.hits[name]++
	ch := f.hold[name]
	delete(f.hold, name)
	delay := f.delay
	f.mu.Unlock()

	w.Header().Set("ETag", `"v1"`)
	if ch != nil {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body[:32*1024])
		w.(http.Flusher).Flush()
		<-ch
		return
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	http.ServeContent(w, r, name, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), bytes.NewReader(body))
}

// holdNext, adi verilen dosyanin bir SONRAKI istegini 32 KB'den sonra askiya alir.
func (f *fakeSite) holdNext(name string) chan struct{} {
	ch := make(chan struct{})
	f.mu.Lock()
	f.hold[name] = ch
	f.mu.Unlock()
	return ch
}

func (f *fakeSite) hitCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[name]
}

// fakeResolver: "album://x" -> tum dosyalar; "file://<ad>" -> tek dosya.
type fakeResolver struct{ f *fakeSite }

func (r *fakeResolver) Match(u string) bool {
	return strings.HasPrefix(u, "album://") || strings.HasPrefix(u, "file://")
}

func (r *fakeResolver) item(name string) site.Item {
	return site.Item{
		URL:        r.f.srv.URL + "/f/" + name,
		SourcePage: "file://" + name,
		Dir:        "Albüm",
		Filename:   name,
		Size:       int64(len(r.f.files[name])),
	}
}

func (r *fakeResolver) Resolve(ctx context.Context, u string, yield func(site.Item) error) ([]site.ItemError, error) {
	if strings.HasPrefix(u, "file://") {
		return nil, yield(r.item(strings.TrimPrefix(u, "file://")))
	}
	names := make([]string, 0, len(r.f.files))
	for n := range r.f.files {
		names = append(names, n)
	}
	// deterministik sira
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for i, n := range names {
		it := r.item(n)
		it.Index = i
		if err := yield(it); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (r *fakeResolver) ResolveOne(ctx context.Context, sourcePage string) (site.Item, error) {
	name := strings.TrimPrefix(sourcePage, "file://")
	if _, ok := r.f.files[name]; !ok {
		return site.Item{}, fmt.Errorf("yok: %s", name)
	}
	return r.item(name), nil
}

func (r *fakeResolver) Diagnose(context.Context) ([]site.LayerResult, error) { return nil, nil }

// --- Yardimcilar ---

type harness struct {
	t      *testing.T
	f      *fakeSite
	e      *Engine
	out    string
	state  string
	mu     sync.Mutex
	events []Job
	cancel context.CancelFunc
	done   chan struct{}
}

func newHarness(t *testing.T, f *fakeSite, statePath string, maxActive int) *harness {
	t.Helper()
	h := &harness{t: t, f: f, out: testutil.TempDir(t), state: statePath}
	cfg := site.SiteConfig{Name: "fake", MaxConcurrent: 8, MaxRetries: 2}.WithDefaults()
	e, err := New(Options{
		StatePath: statePath,
		MaxActive: maxActive,
		Client:    f.srv.Client(),
		Resolvers: []site.Resolver{&fakeResolver{f: f}},
		Configs:   []site.SiteConfig{cfg},
		OnChange: func(j Job) {
			h.mu.Lock()
			h.events = append(h.events, j)
			h.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.e = e
	return h
}

func (h *harness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() { h.e.Run(ctx); close(h.done) }()
}

func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		h.t.Fatal("motor 10 sn'de kapanmadi")
	}
}

// waitState, id'li isin verilen duruma gelmesini bekler.
func (h *harness) waitState(id string, want State, timeout time.Duration) Job {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, j := range h.e.Jobs() {
			if j.ID == id && j.State == want {
				return j
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	var got State
	for _, j := range h.e.Jobs() {
		if j.ID == id {
			got = j.State
		}
	}
	h.t.Fatalf("is %s %s durumuna gelmedi (%v icinde); su an: %s", id, want, timeout, got)
	return Job{}
}

func (h *harness) waitAll(want State, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok := true
		jobs := h.e.Jobs()
		for _, j := range jobs {
			if j.State != want {
				ok = false
				break
			}
		}
		if ok && len(jobs) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("tum isler %s olmadi: %+v", want, states(h.e.Jobs()))
}

func states(jobs []Job) map[string]State {
	m := map[string]State{}
	for _, j := range jobs {
		m[j.Filename] = j.State
	}
	return m
}

func jobByName(e *Engine, name string) Job {
	for _, j := range e.Jobs() {
		if j.Filename == name {
			return j
		}
	}
	return Job{}
}

// --- Testler ---

func TestAddResolvesAndDownloads(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin")
	h := newHarness(t, f, "", 2)
	h.start()
	defer h.stop()

	n, err := h.e.Add(context.Background(), "album://x", h.out)
	if err != nil || n != 3 {
		t.Fatalf("Add: n=%d err=%v", n, err)
	}
	h.waitAll(StateDone, 10*time.Second)

	for name, want := range f.files {
		got, err := os.ReadFile(filepath.Join(h.out, "Albüm", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s icerigi bozuk", name)
		}
	}
	// Kayit yazilmis olmali.
	if _, err := os.Stat(filepath.Join(h.out, "done.jsonl")); err != nil {
		t.Error("kayit dosyasi yok")
	}
	// Olay akisi: her is queued -> running -> done gormeli.
	h.mu.Lock()
	seen := map[string]map[State]bool{}
	for _, ev := range h.events {
		if seen[ev.ID] == nil {
			seen[ev.ID] = map[State]bool{}
		}
		seen[ev.ID][ev.State] = true
	}
	h.mu.Unlock()
	for id, m := range seen {
		if !m[StateQueued] || !m[StateRunning] || !m[StateDone] {
			t.Errorf("is %s icin olaylar eksik: %v", id, m)
		}
	}
}

// Ayni linki iki kez eklemek kuyrugu cogaltmamali.
func TestAddIsIdempotent(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
	h := newHarness(t, f, "", 1)
	// Motoru baslatmadan ekle: isler queued kalsin.
	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	n, err := h.e.Add(context.Background(), "album://x", h.out)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(h.e.Jobs()) != 2 {
		t.Fatalf("ikinci ekleme %d yeni is uretti, kuyrukta %d is", n, len(h.e.Jobs()))
	}
}

// Calisan isi duraklat: .part kalmali, durum Paused; devam edince kaldigi
// yerden bitmeli (sunucu Range gormeli, bastan indirmemeli).
func TestPauseThenResumeContinuesFromPart(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	h := newHarness(t, f, "", 1)
	hold := f.holdNext("a.bin")
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	j := h.waitState(jobByName(h.e, "a.bin").ID, StateRunning, 5*time.Second)
	// Ilk 32 KB'nin diske inmesi icin kisa bir sure.
	time.Sleep(300 * time.Millisecond)

	h.e.Pause(j.ID)
	close(hold)
	j = h.waitState(j.ID, StatePaused, 5*time.Second)

	part := filepath.Join(h.out, "Albüm", "a.bin.part")
	fi, err := os.Stat(part)
	if err != nil {
		t.Fatalf(".part yok: %v", err)
	}
	if fi.Size() <= 0 || fi.Size() >= int64(len(f.files["a.bin"])) {
		t.Fatalf(".part boyutu %d: kismi ilerleme bekleniyordu", fi.Size())
	}
	if j.Path == "" || filepath.Base(j.Path) != "a.bin" {
		t.Errorf("duraklayan isin yolu kaydedilmemis: %q", j.Path)
	}

	h.e.Resume(j.ID)
	h.waitState(j.ID, StateDone, 10*time.Second)

	got, _ := os.ReadFile(filepath.Join(h.out, "Albüm", "a.bin"))
	if !bytes.Equal(got, f.files["a.bin"]) {
		t.Fatal("devam sonrasi icerik bozuk")
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Error("tamamlanan isin .part'i kaldi")
	}
	if f.hitCount("a.bin") != 2 {
		t.Errorf("sunucu %d kez cagrildi, 2 bekleniyordu (1 kesik + 1 devam)", f.hitCount("a.bin"))
	}
}

// Siradaki (henuz baslamamis) isi duraklatmak onu atlamali; digerleri akmali.
func TestPauseQueuedJobIsSkippedByDispatcher(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
	h := newHarness(t, f, "", 1)
	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	a := jobByName(h.e, "a.bin")
	h.e.Pause(a.ID)
	h.start()
	defer h.stop()

	h.waitState(jobByName(h.e, "b.bin").ID, StateDone, 10*time.Second)
	if got := jobByName(h.e, "a.bin").State; got != StatePaused {
		t.Fatalf("duraklatilan is %s oldu, paused kalmaliydi", got)
	}
}

// Kaldir + dosyalari sil: .part ve state gitmeli, is listeden dusmeli.
func TestRemoveRunningJobWipesPartial(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	h := newHarness(t, f, "", 1)
	hold := f.holdNext("a.bin")
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	j := h.waitState(jobByName(h.e, "a.bin").ID, StateRunning, 5*time.Second)
	time.Sleep(300 * time.Millisecond)

	h.e.Remove(j.ID, true)
	close(hold)

	deadline := time.Now().Add(5 * time.Second)
	part := filepath.Join(h.out, "Albüm", "a.bin.part")
	for time.Now().Before(deadline) {
		_, perr := os.Stat(part)
		_, serr := os.Stat(part + ".state")
		if os.IsNotExist(perr) && os.IsNotExist(serr) && len(h.e.Jobs()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("kaldirilan isin dosyalari kaldi veya is listede: %d is", len(h.e.Jobs()))
}

// MaxActive ayni anda calisan is sayisini sinirlamali.
func TestMaxActiveIsRespected(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin", "d.bin", "e.bin")
	f.delay = 150 * time.Millisecond
	h := newHarness(t, f, "", 2)
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	maxSeen := 0
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		running := 0
		allDone := true
		for _, j := range h.e.Jobs() {
			if j.State == StateRunning {
				running++
			}
			if j.State != StateDone {
				allDone = false
			}
		}
		if running > maxSeen {
			maxSeen = running
		}
		if allDone {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if maxSeen > 2 {
		t.Fatalf("ayni anda %d is calisti, en fazla 2 olmaliydi", maxSeen)
	}
	if maxSeen == 0 {
		t.Fatal("hic calisan is gorulmedi")
	}
}

// Kapanis: calisan is iptal edilir, kuyruk dosyasina "queued" yazilir, yeni
// motor onu okuyup kaldigi yerden bitirir.
func TestPersistenceAcrossRestart(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
	statePath := filepath.Join(testutil.TempDir(t), "queue.json")
	h := newHarness(t, f, statePath, 1)
	hold := f.holdNext("a.bin")
	h.start()

	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	h.waitState(jobByName(h.e, "a.bin").ID, StateRunning, 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	h.stop() // kapanis: a.bin iptal edilir
	close(hold)

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("kuyruk dosyasi yok: %v", err)
	}
	if !strings.Contains(string(raw), `"a.bin"`) || !strings.Contains(string(raw), `"b.bin"`) {
		t.Fatalf("kuyruk dosyasi isleri icermiyor: %s", raw)
	}
	if strings.Contains(string(raw), `"running"`) {
		t.Fatal("kapanista running yazilmis; queued olmaliydi")
	}

	// Yeni motor, ayni dosya: iki is de bitmeli, a.bin devam etmeli.
	h2 := newHarness(t, f, statePath, 1)
	h2.out = h.out
	if len(h2.e.Jobs()) != 2 {
		t.Fatalf("yeniden acilista %d is, 2 bekleniyordu", len(h2.e.Jobs()))
	}
	h2.start()
	defer h2.stop()
	h2.waitAll(StateDone, 15*time.Second)
	for name, want := range f.files {
		got, _ := os.ReadFile(filepath.Join(h.out, "Albüm", name))
		if !bytes.Equal(got, want) {
			t.Fatalf("%s yeniden acilis sonrasi bozuk", name)
		}
	}
}

// Kuyruk dosyasi bozuksa uygulama acilmali (bos kuyruk) ve bunu soylemeli.
func TestCorruptStateFileStartsEmpty(t *testing.T) {
	statePath := filepath.Join(testutil.TempDir(t), "queue.json")
	if err := os.WriteFile(statePath, []byte("{bozuk"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFakeSite(t, "a.bin")
	var logged []string
	cfg := site.SiteConfig{Name: "fake"}.WithDefaults()
	e, err := New(Options{
		StatePath: statePath, Client: f.srv.Client(),
		Resolvers: []site.Resolver{&fakeResolver{f: f}}, Configs: []site.SiteConfig{cfg},
		Events: runEvents(func(s string) { logged = append(logged, s) }),
	})
	if err != nil {
		t.Fatalf("bozuk kuyruk motoru acilmaz yapti: %v", err)
	}
	if len(e.Jobs()) != 0 {
		t.Fatal("bozuk dosyadan is okundu")
	}
	if len(logged) == 0 || !strings.Contains(logged[0], "bozuk") {
		t.Errorf("bozuk kuyruk bildirilmedi: %v", logged)
	}
}

// PauseAll yeni is baslatmayi kesmeli; ResumeAll hepsini geri koymali.
func TestPauseAllResumeAll(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin")
	f.delay = 100 * time.Millisecond
	h := newHarness(t, f, "", 1)
	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	h.e.PauseAll()
	h.start()
	defer h.stop()

	time.Sleep(400 * time.Millisecond)
	for _, j := range h.e.Jobs() {
		if j.State == StateRunning || j.State == StateDone {
			t.Fatalf("PauseAll acikken is ilerledi: %s %s", j.Filename, j.State)
		}
	}
	if !h.e.Paused() {
		t.Fatal("Paused() false")
	}
	h.e.ResumeAll()
	h.waitAll(StateDone, 15*time.Second)
}

// Hiz siniri motora yansimali ve kaldirilabilmeli.
func TestSpeedLimitIsApplied(t *testing.T) {
	f := newFakeSite(t, "a.bin") // 96 KB
	h := newHarness(t, f, "", 1)
	h.e.SetSpeedLimit(192 * 1024) // 2 saniyede 96 KB -> ~0.5 sn
	if h.e.SpeedLimit() != 192*1024 {
		t.Fatal("sinir okunamadi")
	}
	h.start()
	defer h.stop()

	start := time.Now()
	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	h.waitState(jobByName(h.e, "a.bin").ID, StateDone, 10*time.Second)
	if el := time.Since(start); el < 350*time.Millisecond {
		t.Fatalf("sinirli indirme %v surdu; sinir uygulanmamis", el)
	}
}

// Ikinci ekleme: zaten inen dosya "skipped" olmali, yeniden inmemeli.
func TestSecondRunSkipsCompleted(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	statePath := filepath.Join(testutil.TempDir(t), "queue.json")
	h := newHarness(t, f, statePath, 1)
	h.start()
	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	h.waitState(jobByName(h.e, "a.bin").ID, StateDone, 10*time.Second)
	h.e.ClearFinished()
	h.stop()

	h2 := newHarness(t, f, statePath, 1)
	h2.out = h.out
	h2.start()
	defer h2.stop()
	if _, err := h2.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	h2.waitState(jobByName(h2.e, "a.bin").ID, StateSkipped, 10*time.Second)
	if f.hitCount("a.bin") != 1 {
		t.Errorf("dosya %d kez indirildi, 1 bekleniyordu", f.hitCount("a.bin"))
	}
}

// Ayni item icin ikinci Add, calisan isi bozmamali.
func TestJobIDIsStable(t *testing.T) {
	a := jobID("E:\\x", "https://s/f/1", "Alb", "a.mp4")
	b := jobID("E:\\x", "https://s/f/1", "Alb", "a.mp4")
	c := jobID("E:\\y", "https://s/f/1", "Alb", "a.mp4")
	if a != b || a == c || len(a) != 16 {
		t.Fatalf("kimlikler: %s %s %s", a, b, c)
	}
}

// runEvents, Errorf'u verilen fonksiyona baglayan minimal Events.
func runEvents(errorf func(string)) run.Events {
	return run.Events{Errorf: func(f string, a ...any) { errorf(fmt.Sprintf(f, a...)) }}
}
