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

	mu     sync.Mutex
	hold   map[string]chan struct{} // ad -> serbest birakma kanali
	hits   map[string]int
	delay  time.Duration
	forbid map[string]bool // ad -> 403 don (captcha simulasyonu)
	quota  bool            // true ise her dosyaya 509 (mega kota simulasyonu)
}

func newFakeSite(t *testing.T, names ...string) *fakeSite {
	t.Helper()
	f := &fakeSite{t: t, files: map[string][]byte{}, hold: map[string]chan struct{}{}, hits: map[string]int{}, forbid: map[string]bool{}}
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
	forbid := f.forbid[name]
	quota := f.quota
	f.mu.Unlock()
	if forbid {
		http.Error(w, `{"value":"file_rate_limited_captcha_required"}`, http.StatusForbidden)
		return
	}
	if quota {
		http.Error(w, "Bandwidth Limit Exceeded", 509)
		return
	}

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

func (f *fakeSite) setQuota(on bool) {
	f.mu.Lock()
	f.quota = on
	f.mu.Unlock()
}

func (f *fakeSite) hitCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[name]
}

// fakeResolver: "album://x" -> tum dosyalar; "file://<ad>" -> tek dosya.
type fakeResolver struct {
	f       *fakeSite
	captcha bool // true ise 403 "captcha gerekli" olarak siniflanir (pixeldrain gibi)
	// quotaWait, 509'a eklenen sifirlanma suresi (mega'nin "uq" cevabi gibi).
	quotaWait time.Duration

	mu          sync.Mutex
	resolveOnes int
	quotaOK     bool // QuotaAvailable'in cevabi ("VPN degisti, pay var")
	probes      int
}

// QuotaAvailable, site.QuotaProber (mega'nin "uq" sorgusu gibi).
func (r *fakeResolver) QuotaAvailable(context.Context) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probes++
	return r.quotaOK, nil
}

func (r *fakeResolver) setQuotaOK(ok bool) {
	r.mu.Lock()
	r.quotaOK = ok
	r.mu.Unlock()
}

func (r *fakeResolver) probeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.probes
}

// fakeCaptchaErr, site.StatusClassifier'in captcha sinyali: politika bunu
// gorunce ErrStop ile kosuyu durdurur.
type fakeCaptchaErr struct{}

func (fakeCaptchaErr) Error() string         { return "captcha gerekli (sahte)" }
func (fakeCaptchaErr) CaptchaRequired() bool { return true }

func (r *fakeResolver) ClassifyStatus(resp *http.Response, _ []byte) error {
	if r.captcha && resp.StatusCode == http.StatusForbidden {
		return fakeCaptchaErr{}
	}
	if resp.StatusCode == 509 {
		return &site.QuotaError{Wait: r.quotaWait, Err: site.Errorf(site.LayerCDN, "fake", "kota doldu (sahte 509)")}
	}
	return nil
}

func (r *fakeResolver) resolveOneCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resolveOnes
}

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
	r.mu.Lock()
	r.resolveOnes++
	r.mu.Unlock()
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

// Kullanici ayari site tavanini asamaz; 1 parcaliyi kapatir.
func TestSetSegmentsRespectsSiteCap(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	cfg := site.SiteConfig{Name: "fake", MaxSegments: 3}.WithDefaults()
	e, err := New(Options{Client: f.srv.Client(), Resolvers: []site.Resolver{&fakeResolver{f: f}}, Configs: []site.SiteConfig{cfg}})
	if err != nil {
		t.Fatal(err)
	}
	// Varsayilan istek 4, tavan 3 -> etkin 3.
	if got := e.workers["fake"].Down.Segments; got != 3 {
		t.Errorf("varsayilanda etkin parca = %d, 3 bekleniyordu", got)
	}
	e.SetSegments(8)
	if got := e.workers["fake"].Down.Segments; got != 3 {
		t.Errorf("8 istenince etkin = %d, tavan 3 olmaliydi", got)
	}
	e.SetSegments(2)
	if got := e.workers["fake"].Down.Segments; got != 2 {
		t.Errorf("2 istenince etkin = %d", got)
	}
	e.SetSegments(0)
	if got := e.workers["fake"].Down.Segments; got != 1 || e.Segments() != 1 {
		t.Errorf("0 istenince etkin = %d, istek = %d; ikisi de 1 olmaliydi", got, e.Segments())
	}
}

// KULLANICININ YASADIGI: captcha kuyrugu durdurdu, VPN degistirdi, ise
// tekrar basti -> is "sirada" gorunuyor ama hic baslamiyordu, cunku genel
// duraklatma acikti. Tek bir isin "devam"i captcha duraklatmasini kaldirmali.
func TestCaptchaPauseIsClearedBySingleResume(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
	f.mu.Lock()
	f.forbid["a.bin"] = true
	f.mu.Unlock()
	h := newHarness(t, f, "", 1)
	// resolver'i captcha kipine al
	h.e.resolvers[0].(*fakeResolver).captcha = true
	h.e.workers["fake"] = newWorkerFor(t, h, true)
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	a := h.waitState(jobByName(h.e, "a.bin").ID, StateStopped, 10*time.Second)
	if !h.e.Paused() || !h.e.PausedByCaptcha() {
		t.Fatal("captcha sonrasi genel duraklatma acilmadi")
	}

	// "VPN degistirdi": sunucu artik izin veriyor. Kullanici tek ise ▶ basiyor.
	f.mu.Lock()
	f.forbid["a.bin"] = false
	f.mu.Unlock()
	h.e.Resume(a.ID)

	h.waitState(a.ID, StateDone, 10*time.Second)
	if h.e.Paused() {
		t.Error("tek isin devami captcha duraklatmasini kaldirmadi")
	}
}

// Kullanicinin KENDI "Tumunu duraklat"i ise tek bir isin devamiyla kalkmamali:
// niyet farkli, digerleri duraklatilmis kalmali.
func TestUserPauseAllSurvivesSingleResume(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
	h := newHarness(t, f, "", 1)
	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	h.e.PauseAll()
	a := jobByName(h.e, "a.bin")
	h.e.Pause(a.ID) // queued -> paused
	h.e.Resume(a.ID)
	if !h.e.Paused() {
		t.Fatal("kullanicinin genel duraklatmasi tek isin devamiyla kalkti")
	}
	if h.e.PausedByCaptcha() {
		t.Fatal("kullanici duraklatmasi captcha sanildi")
	}
}

// newWorkerFor, sahte resolver'in ClassifyStatus'unu tasiyan bir Worker kurar
// (harness varsayilan olarak captcha'siz resolver ile kuruluyor).
func newWorkerFor(t *testing.T, h *harness, captcha bool) *run.Worker {
	t.Helper()
	return newWorkerWith(t, h, &fakeResolver{f: h.f, captcha: captcha})
}

func newWorkerWith(t *testing.T, h *harness, r *fakeResolver) *run.Worker {
	t.Helper()
	cfg := site.SiteConfig{Name: "fake", MaxConcurrent: 8, MaxRetries: 2}.WithDefaults()
	ev := run.Events{Progress: h.e.onProgress}
	return run.NewWorker(r, cfg, h.f.srv.Client(), ev)
}

// --- Kota (mega 509) ---

// Kota dolunca is HATA degil BEKLEME'ye dusmeli, ayni sitenin siradakileri de
// beklemeli (hepsi ayni 509'u alacakti), ve sitenin bildirdigi surede
// kendiliginden yeniden denenmeli. Yeniden deneme TAZE cozumlemeyle yapilir:
// mega'nin indirme adresi isteyen IP'ye bagli.
//
// Kullanicinin gordugu: "hata: mega aktarim kotasi doldu" satirlari oylece
// kaliyor, digerleri inerken bunlar hic denenmiyordu.
func TestQuotaPutsSiteOnHoldAndRetriesAtResetTime(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin")
	f.setQuota(true)
	h := newHarness(t, f, "", 1)
	r := &fakeResolver{f: f, quotaWait: 400 * time.Millisecond}
	h.e.resolvers[0] = r
	h.e.workers["fake"] = newWorkerWith(t, h, r)
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	a := h.waitState(jobByName(h.e, "a.bin").ID, StateWaiting, 10*time.Second)
	if !strings.Contains(a.Error, "kota") {
		t.Errorf("bekleyen isin sebebi yok: %q", a.Error)
	}
	if a.RetryAt.IsZero() {
		t.Error("RetryAt bos")
	}
	// Siradakiler de bekliyor, hem de ayni anda.
	for _, n := range []string{"b.bin", "c.bin"} {
		j := h.waitState(jobByName(h.e, n).ID, StateWaiting, 2*time.Second)
		if !j.RetryAt.Equal(a.RetryAt) {
			t.Errorf("%s RetryAt %v, a ile ayni olmali (%v)", n, j.RetryAt, a.RetryAt)
		}
	}
	if h.e.Paused() {
		t.Error("kota genel duraklatma ACMAMALI; baska siteler devam edebilir")
	}
	before := r.resolveOneCount()

	// Sure doluyor; kota acilmis olsun.
	f.setQuota(false)
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		h.waitState(jobByName(h.e, n).ID, StateDone, 10*time.Second)
	}
	if r.resolveOneCount() <= before {
		t.Error("yeniden denemede taze cozumleme yapilmadi (IP'ye bagli adres eskimis olabilirdi)")
	}
}

// Kullanici VPN degistirip bekleyen tek ise ▶ derse: o is hemen denenir, basarili
// olursa sitenin diger bekleyenleri de serbest kalir (kotanin acildigi kanitlandi).
func TestQuotaResumeOneProbesAndReleasesSiblings(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin")
	f.setQuota(true)
	h := newHarness(t, f, "", 1)
	r := &fakeResolver{f: f, quotaWait: time.Hour}
	h.e.resolvers[0] = r
	h.e.workers["fake"] = newWorkerWith(t, h, r)
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	a := h.waitState(jobByName(h.e, "a.bin").ID, StateWaiting, 10*time.Second)
	h.waitState(jobByName(h.e, "c.bin").ID, StateWaiting, 2*time.Second)

	// VPN degisti, kullanici a'ya ▶ basti.
	f.setQuota(false)
	h.e.Resume(a.ID)
	h.waitState(a.ID, StateDone, 10*time.Second)
	// b ve c bir saat beklememeli.
	h.waitState(jobByName(h.e, "b.bin").ID, StateDone, 10*time.Second)
	h.waitState(jobByName(h.e, "c.bin").ID, StateDone, 10*time.Second)
}

// Kota hala doluyken ▶ denenirse is yeniden beklemeye duser, diger bekleyenler
// serbest KALMAZ; kuyruk sifirlanma saatini korur.
func TestQuotaResumeOneFailingKeepsSiblingsWaiting(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
	f.setQuota(true)
	h := newHarness(t, f, "", 1)
	r := &fakeResolver{f: f, quotaWait: time.Hour}
	h.e.resolvers[0] = r
	h.e.workers["fake"] = newWorkerWith(t, h, r)
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	a := h.waitState(jobByName(h.e, "a.bin").ID, StateWaiting, 10*time.Second)
	b := h.waitState(jobByName(h.e, "b.bin").ID, StateWaiting, 2*time.Second)
	hitsB := f.hitCount("b.bin")

	h.e.Resume(a.ID)
	// a denendi ve yine 509 aldi -> yeniden bekliyor.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.hitCount("a.bin") >= 2 && jobByName(h.e, "a.bin").State == StateWaiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := jobByName(h.e, "a.bin").State; got != StateWaiting {
		t.Fatalf("a %s, yeniden waiting olmaliydi", got)
	}
	if f.hitCount("b.bin") != hitsB {
		t.Error("basarisiz deneme b'yi de bosuna baslatti")
	}
	if got := jobByName(h.e, "b.bin"); got.State != StateWaiting || got.ID != b.ID {
		t.Errorf("b %s, beklemede kalmaliydi", got.State)
	}
}

// MegaBasterd davranisi: kullanici VPN'i degistirir, BASKA HICBIR SEY YAPMAZ,
// indirmeler kendiliginden surer. Kuyruk bekleyen is varken siteye "payim var
// mi" diye soruyor; "evet" gelince bekleyenler taze cozumlemeyle kuyruga doner.
func TestQuotaProbeResumesWithoutUserAction(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin")
	f.setQuota(true)
	h := newHarness(t, f, "", 1)
	h.e.opt.QuotaProbeEvery = 100 * time.Millisecond
	r := &fakeResolver{f: f, quotaWait: time.Hour}
	h.e.resolvers[0] = r
	h.e.workers["fake"] = newWorkerWith(t, h, r)
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		h.waitState(jobByName(h.e, n).ID, StateWaiting, 10*time.Second)
	}
	// Yoklama surmeli ama pay yokken kimse baslamamali.
	time.Sleep(350 * time.Millisecond)
	if r.probeCount() < 2 {
		t.Fatalf("yoklama yapilmiyor: %d", r.probeCount())
	}
	if got := jobByName(h.e, "a.bin").State; got != StateWaiting {
		t.Fatalf("pay yokken is %s oldu", got)
	}
	before := r.resolveOneCount()

	// VPN degisti: CDN izin veriyor, API "pay var" diyor. Kullanici hicbir sey yapmiyor.
	f.setQuota(false)
	r.setQuotaOK(true)
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		h.waitState(jobByName(h.e, n).ID, StateDone, 10*time.Second)
	}
	if r.resolveOneCount() <= before {
		t.Error("salinan isler taze cozumleme yapmadi (adres eski IP'ye bagliydi)")
	}
}

// API "pay var" der ama CDN yine 509 verirse yoklama seyrelmeli (2x, 10 dk
// tavan), sonsuz bir "sal-509-bekle" dongusu her 30 saniyede istek harcamasin.
func TestQuotaProbeBacksOffWhenReleaseFailsAgain(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	f.setQuota(true)
	h := newHarness(t, f, "", 1)
	h.e.opt.QuotaProbeEvery = 100 * time.Millisecond
	r := &fakeResolver{f: f, quotaWait: time.Hour, quotaOK: true} // API hep "var" diyor
	h.e.resolvers[0] = r
	h.e.workers["fake"] = newWorkerWith(t, h, r)
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	id := jobByName(h.e, "a.bin").ID
	h.waitState(id, StateWaiting, 10*time.Second)

	// Ikinci ve ucuncu kez beklemeye dusmesini bekle (sal -> 509 -> bekle).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && f.hitCount("a.bin") < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	if f.hitCount("a.bin") < 3 {
		t.Fatalf("yoklama isi yeniden denemedi: %d istek", f.hitCount("a.bin"))
	}
	h.e.mu.Lock()
	every := h.e.probeEvery["fake"]
	h.e.mu.Unlock()
	if every < 400*time.Millisecond {
		t.Errorf("yoklama seyrelmedi: %s (en az 4x beklenirdi)", every)
	}
}

// Kota komutu (MegaBasterd "509'da komut calistir"): kota dolunca kullanicinin
// komutu BIR kez calisir (ayni anda uc is 509 alsa da), bitince yoklama one
// cekilir ve pay varsa isler ▶ beklemeden surer. Bildirimler OnNotice'a gider.
func TestQuotaCommandRunsOnceAndPullsProbeForward(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin")
	f.setQuota(true)
	h := newHarness(t, f, "", 3)        // uc is ayni anda 509 alsin
	h.e.opt.QuotaProbeEvery = time.Hour // normal yoklama devre disi; komut one cekmeli
	var mu sync.Mutex
	var notices []string
	h.e.opt.OnNotice = func(s string) {
		mu.Lock()
		notices = append(notices, s)
		mu.Unlock()
	}
	r := &fakeResolver{f: f, quotaWait: time.Hour}
	h.e.resolvers[0] = r
	h.e.workers["fake"] = newWorkerWith(t, h, r)
	marker := filepath.Join(testutil.TempDir(t), "vpn calisti.txt")
	// "echo x>> dosya" hem cmd'de hem sh'de calisir; her calisma bir satir ekler.
	h.e.SetQuotaCommand(`echo degisti>> "` + marker + `"`)
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		h.waitState(jobByName(h.e, n).ID, StateWaiting, 10*time.Second)
	}
	// Komut calisip bitmis olmali.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(marker); err == nil && len(b) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("kota komutu calismadi: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(string(b)), "\n") + 1; lines != 1 {
		t.Errorf("komut %d kez calisti, soguma yuzunden 1 bekleniyordu", lines)
	}

	// Komut bitince yoklama one cekilmeli (normalde 1 saat sonraydi).
	f.setQuota(false)
	r.setQuotaOK(true)
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		h.waitState(jobByName(h.e, n).ID, StateDone, 15*time.Second)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(notices, "\n")
	for _, want := range []string{"komut çalıştırılıyor", "Kota komutu bitti", "pay açıldı"} {
		if !strings.Contains(joined, want) {
			t.Errorf("bildirimlerde %q yok:\n%s", want, joined)
		}
	}
}

// Kota dolunca OnQuotaHold BIR kez cagrilmali (uc is ayni anda 509 alsa da);
// arayuz bunu sistem bildirimine ceviriyor.
func TestQuotaHoldNotifiesOnce(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin")
	f.setQuota(true)
	h := newHarness(t, f, "", 3)
	h.e.opt.QuotaProbeEvery = time.Hour
	got := make(chan string, 8)
	h.e.opt.OnQuotaHold = func(site string, at time.Time) {
		if at.IsZero() {
			t.Error("RetryAt bos geldi")
		}
		got <- site
	}
	r := &fakeResolver{f: f, quotaWait: time.Hour}
	h.e.resolvers[0] = r
	h.e.workers["fake"] = newWorkerWith(t, h, r)
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		h.waitState(jobByName(h.e, n).ID, StateWaiting, 10*time.Second)
	}
	select {
	case s := <-got:
		if s != "fake" {
			t.Errorf("site = %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnQuotaHold cagrilmadi")
	}
	select {
	case <-got:
		t.Error("OnQuotaHold ikinci kez cagrildi; site basina 10 dk'da bir olmali")
	case <-time.After(300 * time.Millisecond):
	}
}

// Basarisiz komut kuyrugu bozmamali: is beklemede kalir, bildirim sebebi ve
// komutun ciktisini soyler, sonraki kota yine (sogumadan sonra) deneyebilir.
func TestQuotaCommandFailureIsReported(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	f.setQuota(true)
	h := newHarness(t, f, "", 1)
	h.e.opt.QuotaProbeEvery = time.Hour
	got := make(chan string, 8)
	h.e.opt.OnNotice = func(s string) { got <- s }
	r := &fakeResolver{f: f, quotaWait: time.Hour}
	h.e.resolvers[0] = r
	h.e.workers["fake"] = newWorkerWith(t, h, r)
	h.e.SetQuotaCommand("echo vpn yok&& exit 7")
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	id := jobByName(h.e, "a.bin").ID
	h.waitState(id, StateWaiting, 10*time.Second)

	var failure string
	deadline := time.After(5 * time.Second)
	for failure == "" {
		select {
		case n := <-got:
			if strings.Contains(n, "başarısız") {
				failure = n
			}
		case <-deadline:
			t.Fatal("basarisizlik bildirimi gelmedi")
		}
	}
	if !strings.Contains(failure, "vpn yok") {
		t.Errorf("bildirimde komut ciktisi yok: %q", failure)
	}
	if got := jobByName(h.e, "a.bin").State; got != StateWaiting {
		t.Errorf("basarisiz komut isin durumunu bozdu: %s", got)
	}
}

// Bekleme uygulama kapanip acilinca yerinde kalmali: RetryAt diske yaziliyor,
// suresi gecmisse acilista kuyruga doner.
func TestQuotaWaitSurvivesRestart(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	f.setQuota(true)
	state := filepath.Join(testutil.TempDir(t), "queue.json")
	h := newHarness(t, f, state, 1)
	r := &fakeResolver{f: f, quotaWait: time.Hour}
	h.e.resolvers[0] = r
	h.e.workers["fake"] = newWorkerWith(t, h, r)
	h.start()
	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	a := h.waitState(jobByName(h.e, "a.bin").ID, StateWaiting, 10*time.Second)
	h.stop()

	jobs, err := load(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != StateWaiting || !jobs[0].RetryAt.Equal(a.RetryAt) {
		t.Fatalf("yeniden yuklenen is: %+v", jobs[0])
	}

	// Sifirlanma zamani gecmis olsun: acilista dogrudan kuyruga donmeli ve inmeli.
	jobs[0].RetryAt = time.Now().Add(-time.Second)
	if err := save(state, jobs); err != nil {
		t.Fatal(err)
	}
	f.setQuota(false)
	h2 := newHarness(t, f, state, 1)
	h2.out = h.out
	h2.start()
	defer h2.stop()
	h2.waitState(a.ID, StateDone, 10*time.Second)
}
