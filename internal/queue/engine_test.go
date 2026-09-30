package queue

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

// --- Fake site ---
//
// A single httptest server serves N files; each file supports Range. With
// "hold" a file's body can be suspended after the first chunk (for pause and
// shutdown tests).

type fakeSite struct {
	t     *testing.T
	srv   *httptest.Server
	files map[string][]byte // name -> content

	mu     sync.Mutex
	hold   map[string]chan struct{} // name -> release channel
	hits   map[string]int
	delay  time.Duration
	forbid map[string]bool // name -> return 403 (captcha simulation)
	quota  bool            // if true, 509 for every file (mega quota simulation)
	// ranges records every request's Range header per file.
	ranges map[string][]string
	// gen plays mega's IP-bound URLs: a URL handed out before the last
	// switchIP (an older ?gen=) is answered with 403.
	gen int
}

func newFakeSite(t *testing.T, names ...string) *fakeSite {
	t.Helper()
	f := &fakeSite{t: t, files: map[string][]byte{}, hold: map[string]chan struct{}{}, hits: map[string]int{}, forbid: map[string]bool{}, ranges: map[string][]string{}}
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
	f.ranges[name] = append(f.ranges[name], r.Header.Get("Range"))
	stale := r.URL.Query().Get("gen") != fmt.Sprint(f.gen)
	ch := f.hold[name]
	delete(f.hold, name)
	delay := f.delay
	forbid := f.forbid[name]
	quota := f.quota
	f.mu.Unlock()
	if stale {
		http.Error(w, "URL bound to another IP", http.StatusForbidden)
		return
	}
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

// holdNext suspends the NEXT request for the named file after 32 KB.
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

// switchIP plays a VPN switch: the quota is open again and every URL handed out
// so far expires (mega binds the download URL to the requesting IP).
func (f *fakeSite) switchIP() {
	f.mu.Lock()
	f.gen++
	f.quota = false
	f.mu.Unlock()
}

// rangesOf returns the Range headers of every request made for the file.
func (f *fakeSite) rangesOf(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ranges[name]...)
}

func (f *fakeSite) hitCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[name]
}

// fakeResolver: "album://x" -> every file; "file://<name>" -> a single file.
type fakeResolver struct {
	f       *fakeSite
	captcha bool // if true a 403 is classified as "captcha required" (like pixeldrain)
	// quotaWait is the reset time attached to the 509 (like mega's "uq" answer).
	quotaWait time.Duration

	mu          sync.Mutex
	resolveOnes int
	// failOnes/failErr: the next failOnes ResolveOne calls return failErr
	// (no network, file deleted...).
	failOnes int
	failErr  error
	// If prefix is set only "<prefix>album://" and "<prefix>file://" are
	// recognized: to build two different sites in the same engine.
	prefix string
	// dir is the album folder of every item; "" means "Album".
	dir string
}

// failNextResolveOne fails the next n re-resolutions with err.
func (r *fakeResolver) failNextResolveOne(n int, err error) {
	r.mu.Lock()
	r.failOnes, r.failErr = n, err
	r.mu.Unlock()
}

// fakeCaptchaErr is site.StatusClassifier's captcha signal: seeing it, the
// policy stops the run with ErrStop.
type fakeCaptchaErr struct{}

func (fakeCaptchaErr) Error() string         { return "captcha required (fake)" }
func (fakeCaptchaErr) CaptchaRequired() bool { return true }

func (r *fakeResolver) ClassifyStatus(resp *http.Response, _ []byte) error {
	if r.captcha && resp.StatusCode == http.StatusForbidden {
		return fakeCaptchaErr{}
	}
	if resp.StatusCode == 509 {
		return &site.QuotaError{Wait: r.quotaWait, Err: site.Errorf(site.LayerCDN, "fake", "quota exceeded (fake 509)")}
	}
	return nil
}

func (r *fakeResolver) resolveOneCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resolveOnes
}

func (r *fakeResolver) Match(u string) bool {
	if !strings.HasPrefix(u, r.prefix) {
		return false
	}
	u = strings.TrimPrefix(u, r.prefix)
	return strings.HasPrefix(u, "album://") || strings.HasPrefix(u, "file://")
}

// item resolves a file; its URL carries the current "IP" (see fakeSite.gen).
func (r *fakeResolver) item(name string) site.Item {
	r.f.mu.Lock()
	gen := r.f.gen
	r.f.mu.Unlock()
	dir := r.dir
	if dir == "" {
		dir = "Album"
	}
	return site.Item{
		URL:        r.f.srv.URL + "/f/" + name + "?gen=" + fmt.Sprint(gen),
		SourcePage: "file://" + name,
		Dir:        dir,
		Filename:   name,
		Size:       int64(len(r.f.files[name])),
	}
}

func (r *fakeResolver) Resolve(ctx context.Context, u string, yield func(site.Item) error) ([]site.ItemError, error) {
	u = strings.TrimPrefix(u, r.prefix)
	if strings.HasPrefix(u, "file://") {
		return nil, yield(r.item(strings.TrimPrefix(u, "file://")))
	}
	names := make([]string, 0, len(r.f.files))
	for n := range r.f.files {
		names = append(names, n)
	}
	// deterministic order
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
	if r.failOnes > 0 {
		r.failOnes--
		err := r.failErr
		r.mu.Unlock()
		return site.Item{}, err
	}
	r.mu.Unlock()
	name := strings.TrimPrefix(sourcePage, "file://")
	if _, ok := r.f.files[name]; !ok {
		return site.Item{}, fmt.Errorf("missing: %s", name)
	}
	return r.item(name), nil
}

func (r *fakeResolver) Diagnose(context.Context) ([]site.LayerResult, error) { return nil, nil }

// --- Helpers ---

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
		h.t.Fatal("the engine did not shut down within 10 s")
	}
}

// waitState waits for the job with id to reach the given state.
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
	h.t.Fatalf("job %s did not reach %s (within %v); now: %s", id, want, timeout, got)
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
	h.t.Fatalf("not every job reached %s: %+v", want, states(h.e.Jobs()))
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

// --- Tests ---

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
		got, err := os.ReadFile(filepath.Join(h.out, "Album", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s content is corrupt", name)
		}
	}
	// The ledger must have been written.
	if _, err := os.Stat(filepath.Join(h.out, "done.jsonl")); err != nil {
		t.Error("no ledger file")
	}
	// Event flow: every job must see queued -> running -> done.
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
			t.Errorf("missing events for job %s: %v", id, m)
		}
	}
}

// Adding the same link twice must not duplicate the queue.
func TestAddIsIdempotent(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
	h := newHarness(t, f, "", 1)
	// Add without starting the engine: the jobs stay queued.
	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	n, err := h.e.Add(context.Background(), "album://x", h.out)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(h.e.Jobs()) != 2 {
		t.Fatalf("the second add produced %d new jobs, %d jobs in the queue", n, len(h.e.Jobs()))
	}
}

// Pause a running job: the .part must stay, the state Paused; on resume it
// must finish from where it stopped (the server must see a Range, no
// download from scratch).
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
	// A short while for the first 32 KB to reach the disk.
	time.Sleep(300 * time.Millisecond)

	h.e.Pause(j.ID)
	close(hold)
	j = h.waitState(j.ID, StatePaused, 5*time.Second)

	part := filepath.Join(h.out, "Album", "a.bin.part")
	fi, err := os.Stat(part)
	if err != nil {
		t.Fatalf("no .part: %v", err)
	}
	if fi.Size() <= 0 || fi.Size() >= int64(len(f.files["a.bin"])) {
		t.Fatalf(".part size %d: partial progress expected", fi.Size())
	}
	if j.Path == "" || filepath.Base(j.Path) != "a.bin" {
		t.Errorf("the paused job's path was not recorded: %q", j.Path)
	}

	h.e.Resume(j.ID)
	h.waitState(j.ID, StateDone, 10*time.Second)

	got, _ := os.ReadFile(filepath.Join(h.out, "Album", "a.bin"))
	if !bytes.Equal(got, f.files["a.bin"]) {
		t.Fatal("content corrupted after resume")
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Error("the completed job's .part remained")
	}
	if f.hitCount("a.bin") != 2 {
		t.Errorf("the server was called %d times, want 2 (1 cut + 1 resume)", f.hitCount("a.bin"))
	}
}

// Pausing a queued (not yet started) job must skip it; the others must flow.
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
		t.Fatalf("the paused job became %s, it should have stayed paused", got)
	}
}

// Remove + delete files: the .part and state must go, the job must drop from the list.
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
	part := filepath.Join(h.out, "Album", "a.bin.part")
	for time.Now().Before(deadline) {
		_, perr := os.Stat(part)
		_, serr := os.Stat(part + ".state")
		if os.IsNotExist(perr) && os.IsNotExist(serr) && len(h.e.Jobs()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the removed job's files remained or the job is still listed: %d jobs", len(h.e.Jobs()))
}

// MaxActive must limit the number of jobs running at the same time.
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
		t.Fatalf("%d jobs ran at the same time, should be at most 2", maxSeen)
	}
	if maxSeen == 0 {
		t.Fatal("no running job was ever seen")
	}
}

// Shutdown: the running job is canceled, "queued" is written to the queue
// file, and a new engine reads it and finishes from where it stopped.
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
	h.stop() // shutdown: a.bin is canceled
	close(hold)

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("no queue file: %v", err)
	}
	if !strings.Contains(string(raw), `"a.bin"`) || !strings.Contains(string(raw), `"b.bin"`) {
		t.Fatalf("the queue file doesn't contain the jobs: %s", raw)
	}
	if strings.Contains(string(raw), `"running"`) {
		t.Fatal("running was written at shutdown; it should have been queued")
	}

	// A new engine, the same file: both jobs must finish, a.bin must continue.
	h2 := newHarness(t, f, statePath, 1)
	h2.out = h.out
	if len(h2.e.Jobs()) != 2 {
		t.Fatalf("%d jobs on reopen, want 2", len(h2.e.Jobs()))
	}
	h2.start()
	defer h2.stop()
	h2.waitAll(StateDone, 15*time.Second)
	for name, want := range f.files {
		got, _ := os.ReadFile(filepath.Join(h.out, "Album", name))
		if !bytes.Equal(got, want) {
			t.Fatalf("%s is corrupt after the reopen", name)
		}
	}
}

// If the queue file is corrupt the app must open (empty queue) and say so.
func TestCorruptStateFileStartsEmpty(t *testing.T) {
	statePath := filepath.Join(testutil.TempDir(t), "queue.json")
	if err := os.WriteFile(statePath, []byte("{corrupt"), 0o600); err != nil {
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
		t.Fatalf("a corrupt queue kept the engine from opening: %v", err)
	}
	if len(e.Jobs()) != 0 {
		t.Fatal("jobs were read from a corrupt file")
	}
	if len(logged) == 0 || !strings.Contains(logged[0], "corrupt") {
		t.Errorf("the corrupt queue was not reported: %v", logged)
	}
}

// PauseAll must stop starting new jobs; ResumeAll must put them all back.
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
			t.Fatalf("a job progressed while PauseAll was on: %s %s", j.Filename, j.State)
		}
	}
	if !h.e.Paused() {
		t.Fatal("Paused() false")
	}
	h.e.ResumeAll()
	h.waitAll(StateDone, 15*time.Second)
}

// The speed limit must reach the engine and be removable.
func TestSpeedLimitIsApplied(t *testing.T) {
	f := newFakeSite(t, "a.bin") // 96 KB
	h := newHarness(t, f, "", 1)
	h.e.SetSpeedLimit(192 * 1024) // 96 KB in 2 seconds -> ~0.5 s
	if h.e.SpeedLimit() != 192*1024 {
		t.Fatal("could not read the limit")
	}
	h.start()
	defer h.stop()

	start := time.Now()
	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	h.waitState(jobByName(h.e, "a.bin").ID, StateDone, 10*time.Second)
	if el := time.Since(start); el < 350*time.Millisecond {
		t.Fatalf("the limited download took %v; the limit was not applied", el)
	}
}

// Second add: an already downloaded file must be "skipped", not downloaded again.
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
		t.Errorf("the file was downloaded %d times, want 1", f.hitCount("a.bin"))
	}
}

// A second Add for the same item must not break the running job.
func TestJobIDIsStable(t *testing.T) {
	a := jobID("E:\\x", "https://s/f/1", "Alb", "a.mp4")
	b := jobID("E:\\x", "https://s/f/1", "Alb", "a.mp4")
	c := jobID("E:\\y", "https://s/f/1", "Alb", "a.mp4")
	if a != b || a == c || len(a) != 16 {
		t.Fatalf("ids: %s %s %s", a, b, c)
	}
}

// runEvents is a minimal Events wiring Errorf to the given function.
func runEvents(errorf func(string)) run.Events {
	return run.Events{Errorf: func(f string, a ...any) { errorf(fmt.Sprintf(f, a...)) }}
}

// The user setting can't exceed the site ceiling; 1 turns segmenting off.
func TestSetSegmentsRespectsSiteCap(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	cfg := site.SiteConfig{Name: "fake", MaxSegments: 3}.WithDefaults()
	e, err := New(Options{Client: f.srv.Client(), Resolvers: []site.Resolver{&fakeResolver{f: f}}, Configs: []site.SiteConfig{cfg}})
	if err != nil {
		t.Fatal(err)
	}
	// Default request 4, ceiling 3 -> effective 3.
	if got := e.workers["fake"].Down.Segments; got != 3 {
		t.Errorf("effective segments by default = %d, want 3", got)
	}
	e.SetSegments(8)
	if got := e.workers["fake"].Down.Segments; got != 3 {
		t.Errorf("effective with 8 requested = %d, the ceiling 3 should apply", got)
	}
	e.SetSegments(2)
	if got := e.workers["fake"].Down.Segments; got != 2 {
		t.Errorf("effective with 2 requested = %d", got)
	}
	e.SetSegments(0)
	if got := e.workers["fake"].Down.Segments; got != 1 || e.Segments() != 1 {
		t.Errorf("effective with 0 requested = %d, requested = %d; both should be 1", got, e.Segments())
	}
}

// WHAT THE USER EXPERIENCED: a captcha stopped the queue, they switched VPN
// and pressed the job again -> it showed "queued" but never started, because
// the global pause was on. Resuming a single job must lift the captcha hold.
func TestCaptchaPauseIsClearedBySingleResume(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
	f.mu.Lock()
	f.forbid["a.bin"] = true
	f.mu.Unlock()
	h := newHarness(t, f, "", 1)
	// put the resolver into captcha mode
	h.e.resolvers[0].(*fakeResolver).captcha = true
	h.e.workers["fake"] = newWorkerFor(t, h, true)
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	a := h.waitState(jobByName(h.e, "a.bin").ID, StateStopped, 10*time.Second)
	if got := h.e.CaptchaHeld(); len(got) != 1 || got[0] != "fake" {
		t.Fatalf("the site was not held after the captcha: %v", got)
	}
	// A new job of the same site must not start while the hold lasts.
	if _, err := h.e.Add(context.Background(), "file://b.bin", h.out); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if f.hitCount("b.bin") != 0 {
		t.Fatal("a job of the same site started while the captcha hold was on")
	}

	// "They switched VPN": the server allows it now. The user presses ▶ on the single job.
	f.mu.Lock()
	f.forbid["a.bin"] = false
	f.mu.Unlock()
	h.e.Resume(a.ID)

	h.waitState(a.ID, StateDone, 10*time.Second)
	h.waitState(jobByName(h.e, "b.bin").ID, StateDone, 10*time.Second)
	if got := h.e.CaptchaHeld(); len(got) != 0 {
		t.Errorf("resuming a single job did not lift the captcha hold: %v", got)
	}
}

// A captcha is SITE-specific (pixeldrain's per-file counter): a global pause
// used to be switched on and other sites' (mega, bunkr) jobs stopped for nothing.
func TestCaptchaHoldsOnlyThatSite(t *testing.T) {
	fa := newFakeSite(t, "a.bin", "a2.bin")
	fb := newFakeSite(t, "b.bin")
	fa.forbid["a.bin"] = true
	ra := &fakeResolver{f: fa, captcha: true, prefix: "px+"}
	rb := &fakeResolver{f: fb, prefix: "mg+"}
	e, err := New(Options{
		MaxActive: 1,
		Client:    fa.srv.Client(),
		Resolvers: []site.Resolver{ra, rb},
		Configs: []site.SiteConfig{
			site.SiteConfig{Name: "px", MaxConcurrent: 8, MaxRetries: 2}.WithDefaults(),
			site.SiteConfig{Name: "mg", MaxConcurrent: 8, MaxRetries: 2}.WithDefaults(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, e: e, out: testutil.TempDir(t)}
	h.start()
	defer h.stop()

	if _, err := e.Add(context.Background(), "px+file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	h.waitState(jobByName(e, "a.bin").ID, StateStopped, 10*time.Second)

	for _, u := range []string{"px+file://a2.bin", "mg+file://b.bin"} {
		if _, err := e.Add(context.Background(), u, h.out); err != nil {
			t.Fatal(err)
		}
	}
	h.waitState(jobByName(e, "b.bin").ID, StateDone, 10*time.Second)
	if e.Paused() {
		t.Error("the captcha switched on the global pause")
	}
	if fa.hitCount("a2.bin") != 0 {
		t.Error("a job of the site under captcha started")
	}
	if got := e.CaptchaHeld(); len(got) != 1 || got[0] != "px" {
		t.Errorf("held sites %v, want [px]", got)
	}
}

// The user's OWN "Pause all", however, must not lift with a single job's
// resume: the intent differs, the others must stay paused.
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
		t.Fatal("the user's global pause lifted with a single job's resume")
	}
	if len(h.e.CaptchaHeld()) != 0 {
		t.Fatal("the user's pause was taken for a captcha")
	}
}

// --- Network outage ---

// netDown is the error when the local network is unavailable (Wi-Fi connecting, VPN switching).
var netDown = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connectex: network is unreachable")}

// forgetItems forgets the resolved items as if the app had been reopened:
// jobs have to go through ResolveOne when they start.
func forgetItems(e *Engine) {
	e.mu.Lock()
	e.items = map[string]site.Item{}
	e.mu.Unlock()
}

func (h *harness) sawState(id string, s State) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ev := range h.events {
		if ev.ID == id && ev.State == s {
			return true
		}
	}
	return false
}

// WHAT THE USER WOULD EXPERIENCE: the app opened before the network. Every
// job used to fall into "failed" at ResolveOne right away, the next started
// and fell too; within seconds the whole queue was "failed" and each had to be
// pressed ▶ one by one. A network error must not fail the job: it must stay
// queued and wait.
func TestTransientResolveErrorKeepsJobsQueued(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
	h := newHarness(t, f, "", 1)
	h.e.opt.NetRetryBase = 20 * time.Millisecond
	var mu sync.Mutex
	var notices []string
	h.e.opt.OnNotice = func(s string) { mu.Lock(); notices = append(notices, s); mu.Unlock() }
	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	forgetItems(h.e)
	h.e.workers["fake"].Resolver.(*fakeResolver).failNextResolveOne(3, netDown)
	h.start()
	defer h.stop()

	h.waitAll(StateDone, 10*time.Second)
	for _, j := range h.e.Jobs() {
		if h.sawState(j.ID, StateFailed) {
			t.Errorf("%s fell into 'failed' on a network error", j.Filename)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, n := range notices {
		if strings.Contains(n, "network") {
			found = true
		}
	}
	if !found {
		t.Errorf("the network error was not reported to the user: %v", notices)
	}
}

// A real answer from the server (file deleted) is NOT a network error:
// waiting doesn't fix it, "failed" right away as before.
func TestPermanentResolveErrorStillFails(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	h := newHarness(t, f, "", 1)
	h.e.opt.NetRetryBase = 20 * time.Millisecond
	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	forgetItems(h.e)
	r := h.e.workers["fake"].Resolver.(*fakeResolver)
	r.failNextResolveOne(100, errors.New("file deleted"))
	h.start()
	defer h.stop()
	a := h.waitState(jobByName(h.e, "a.bin").ID, StateFailed, 5*time.Second)
	if got := r.resolveOneCount(); got != 1 {
		t.Errorf("the permanent error was tried %d times, want 1", got)
	}

	// "Retry failed": the file is back.
	r.failNextResolveOne(0, nil)
	h.e.RetryFailed()
	h.waitState(a.ID, StateDone, 10*time.Second)
}

// If the network never comes back the job must not stay "queued" forever:
// when the backoff budget runs out it falls into "failed" as before and the
// user sees it.
func TestTransientResolveGivesUpEventually(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	h := newHarness(t, f, "", 1)
	h.e.opt.NetRetryBase = time.Millisecond
	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	forgetItems(h.e)
	r := h.e.workers["fake"].Resolver.(*fakeResolver)
	r.failNextResolveOne(1000, netDown)
	h.start()
	defer h.stop()
	h.waitState(jobByName(h.e, "a.bin").ID, StateFailed, 10*time.Second)
	if got := r.resolveOneCount(); got != maxNetFails+1 {
		t.Errorf("%d attempts made, want %d", got, maxNetFails+1)
	}
}

// newWorkerFor builds a Worker carrying the fake resolver's ClassifyStatus
// (the harness is built with a captcha-free resolver by default).
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

// --- Quota (mega 509) ---

// When the quota runs out the job must fall into WAITING, not FAILED; the
// same site's queued jobs must wait too (they'd all get the same 509), and it
// must be retried by itself at the time the site reported. The retry is done
// with a FRESH resolution: mega's download URL is bound to the requesting IP.
//
// What the user saw: "failed: mega transfer quota exceeded" rows just sat
// there and were never retried while the others downloaded.
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
	if !strings.Contains(a.Error, "quota") {
		t.Errorf("the waiting job has no reason: %q", a.Error)
	}
	if a.RetryAt.IsZero() {
		t.Error("RetryAt is empty")
	}
	// The queued ones wait too, all at the same time.
	for _, n := range []string{"b.bin", "c.bin"} {
		j := h.waitState(jobByName(h.e, n).ID, StateWaiting, 2*time.Second)
		if !j.RetryAt.Equal(a.RetryAt) {
			t.Errorf("%s RetryAt %v, should equal a's (%v)", n, j.RetryAt, a.RetryAt)
		}
	}
	if h.e.Paused() {
		t.Error("a quota must NOT switch on the global pause; other sites can continue")
	}
	before := r.resolveOneCount()

	// The time runs out; let the quota be open.
	f.setQuota(false)
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		h.waitState(jobByName(h.e, n).ID, StateDone, 10*time.Second)
	}
	if r.resolveOneCount() <= before {
		t.Error("the retry did no fresh resolution (the IP-bound URL could have gone stale)")
	}
}

// If the user switches VPN and presses ▶ on a single waiting job: that job is
// tried right away, and if it succeeds the site's other waiting jobs are
// released too (the quota was proven open).
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

	// The VPN switched, the user pressed ▶ on a.
	f.setQuota(false)
	h.e.Resume(a.ID)
	h.waitState(a.ID, StateDone, 10*time.Second)
	// b and c must not wait an hour.
	h.waitState(jobByName(h.e, "b.bin").ID, StateDone, 10*time.Second)
	h.waitState(jobByName(h.e, "c.bin").ID, StateDone, 10*time.Second)
}

// If ▶ is tried while the quota is still full, the job falls back into
// waiting, the other waiting jobs are NOT released; the queue keeps the reset
// time.
func TestQuotaResumeOneFailingKeepsSiblingsWaiting(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
	f.setQuota(true)
	h := newHarness(t, f, "", 1)
	h.e.opt.QuotaProbeEvery = time.Hour // only the user's ▶ may touch the files here
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
	// a was tried and got 509 again -> waiting again.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.hitCount("a.bin") >= 2 && jobByName(h.e, "a.bin").State == StateWaiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := jobByName(h.e, "a.bin").State; got != StateWaiting {
		t.Fatalf("a is %s, it should be waiting again", got)
	}
	if f.hitCount("b.bin") != hitsB {
		t.Error("the failed attempt started b for nothing too")
	}
	if got := jobByName(h.e, "b.bin"); got.State != StateWaiting || got.ID != b.ID {
		t.Errorf("b is %s, it should have stayed waiting", got.State)
	}
}

// MegaBasterd behavior: the user switches the VPN and DOES NOTHING ELSE,
// downloads continue by themselves. While jobs wait for quota the queue asks
// for the first byte of a waiting file every QuotaProbeEvery; as soon as one
// comes back, EVERY waiting job returns to the queue at once. Nothing waits
// for a whole file to download first (the probe used to be a full download
// of the smallest file, and the rest waited until it finished).
//
// The API isn't asked "do I have allowance": in a live measurement "uq" gave
// the same answer with the quota full and empty.
func TestQuotaProbeReleasesAllWaitingAtOnce(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin")
	f.setQuota(true)
	h := newHarness(t, f, "", 3)
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
	// While the quota is full the probes ask for ONE BYTE, and nothing else
	// is requested: no job starts, no file is downloaded to find out.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(probesOf(f, "a.bin", "b.bin", "c.bin")) < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	if n := len(probesOf(f, "a.bin", "b.bin", "c.bin")); n < 3 {
		t.Fatalf("%d one-byte probes while the quota was full; probing doesn't happen", n)
	}
	for _, j := range h.e.Jobs() {
		if j.State != StateWaiting {
			t.Fatalf("%s is %s while the quota is full; probing must not start jobs", j.Filename, j.State)
		}
	}

	// The quota opens (time passed, or the VPN changed). The user does nothing.
	h.mu.Lock()
	h.events = nil
	h.mu.Unlock()
	f.setQuota(false)
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		h.waitState(jobByName(h.e, n).ID, StateDone, 10*time.Second)
	}
	// All three went back to the queue BEFORE any of them finished.
	h.mu.Lock()
	defer h.mu.Unlock()
	requeued := map[string]bool{}
	for _, ev := range h.events {
		if ev.State == StateDone {
			break
		}
		if ev.State == StateQueued || ev.State == StateRunning {
			requeued[ev.Filename] = true
		}
	}
	if len(requeued) != 3 {
		t.Errorf("only %v went back to the queue before the first file finished; all three should have", requeued)
	}
}

// probesOf returns the one-byte probe requests made for the given files.
func probesOf(f *fakeSite, names ...string) []string {
	var out []string
	for _, n := range names {
		for _, r := range f.rangesOf(n) {
			if r == "bytes=0-0" {
				out = append(out, n)
			}
		}
	}
	return out
}

// While the quota stays full, probing keeps a fixed, short interval: the user
// may switch the VPN at any moment (it used to back off to 10 minutes). The
// probe reuses its resolved URL instead of asking the API every time.
func TestQuotaProbeKeepsFixedCadence(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	f.setQuota(true)
	h := newHarness(t, f, "", 1)
	h.e.opt.QuotaProbeEvery = 100 * time.Millisecond
	r := &fakeResolver{f: f, quotaWait: time.Hour}
	h.e.resolvers[0] = r
	h.e.workers["fake"] = newWorkerWith(t, h, r)
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	id := jobByName(h.e, "a.bin").ID
	h.waitState(id, StateWaiting, 10*time.Second)
	resolvesBefore := r.resolveOneCount()

	time.Sleep(1200 * time.Millisecond)
	probes := len(probesOf(f, "a.bin"))
	if probes < 6 {
		t.Errorf("%d probes in 1.2 s at a 100 ms interval; the interval must not grow", probes)
	}
	if got := r.resolveOneCount() - resolvesBefore; got > 1 {
		t.Errorf("%d resolutions for %d probes; the probe should reuse its URL", got, probes)
	}
	if got := jobByName(h.e, "a.bin").State; got != StateWaiting {
		t.Errorf("state is %s, should still be waiting", got)
	}
}

// A VPN switch: the quota is open again, and the URL the probe kept is bound
// to the old IP (403). The probe must resolve it again at once and release
// the jobs, without waiting for another round.
func TestQuotaProbeFollowsVPNSwitch(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
	f.setQuota(true)
	h := newHarness(t, f, "", 2)
	h.e.opt.QuotaProbeEvery = 100 * time.Millisecond
	r := &fakeResolver{f: f, quotaWait: time.Hour}
	h.e.resolvers[0] = r
	h.e.workers["fake"] = newWorkerWith(t, h, r)
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	h.waitState(jobByName(h.e, "a.bin").ID, StateWaiting, 10*time.Second)
	h.waitState(jobByName(h.e, "b.bin").ID, StateWaiting, 10*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(probesOf(f, "a.bin", "b.bin")) < 2 {
		time.Sleep(10 * time.Millisecond)
	}

	f.switchIP()
	for _, n := range []string{"a.bin", "b.bin"} {
		h.waitState(jobByName(h.e, n).ID, StateDone, 10*time.Second)
	}
	for _, n := range []string{"a.bin", "b.bin"} {
		got, _ := os.ReadFile(filepath.Join(h.out, "Album", n))
		if !bytes.Equal(got, f.files[n]) {
			t.Errorf("%s is corrupt", n)
		}
	}
}

// If the probed file itself is gone (a permanent error, not the quota) that
// job fails and probing moves on to another waiting file; a dead file at the
// head of the queue must not keep the site waiting until the reset time.
func TestQuotaProbeSkipsDeadFile(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
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
	a := h.waitState(jobByName(h.e, "a.bin").ID, StateWaiting, 10*time.Second)
	h.waitState(jobByName(h.e, "b.bin").ID, StateWaiting, 2*time.Second)

	// a now gives a permanent error (403 even with a fresh URL).
	f.mu.Lock()
	f.forbid["a.bin"] = true
	f.mu.Unlock()
	h.waitState(a.ID, StateFailed, 5*time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(probesOf(f, "b.bin")) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(probesOf(f, "b.bin")) == 0 {
		t.Fatal("after the probed file failed, the site's other waiting file was never probed")
	}
	f.setQuota(false)
	h.waitState(jobByName(h.e, "b.bin").ID, StateDone, 10*time.Second)
}

// Quota command (MegaBasterd's "run command on 509"): when the quota runs out
// the user's command runs ONCE (even if three jobs get 509 at the same time),
// when it finishes the probe is pulled forward and if there is allowance the
// jobs go on without ▶. Notifications go to OnNotice.
func TestQuotaCommandRunsOnceAndPullsProbeForward(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin")
	f.setQuota(true)
	h := newHarness(t, f, "", 3)        // three jobs get 509 at the same time
	h.e.opt.QuotaProbeEvery = time.Hour // normal probing off; the command must pull it forward
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
	marker := filepath.Join(testutil.TempDir(t), "vpn ran.txt")
	// "echo x>> file" works in both cmd and sh; every run appends a line.
	h.e.SetQuotaCommand(`echo switched>> "` + marker + `"`)
	h.start()
	defer h.stop()

	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		h.waitState(jobByName(h.e, n).ID, StateWaiting, 10*time.Second)
	}
	// The command must have run and finished.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(marker); err == nil && len(b) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the quota command did not run: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(string(b)), "\n") + 1; lines != 1 {
		t.Errorf("the command ran %d times, 1 was expected because of the cooldown", lines)
	}

	// When the command finishes the probe must be pulled forward (it was an hour away).
	f.setQuota(false)
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		h.waitState(jobByName(h.e, n).ID, StateDone, 15*time.Second)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(notices, "\n")
	for _, want := range []string{"running the command", "Quota command finished", "allowance available again"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q missing from the notifications:\n%s", want, joined)
		}
	}
}

// When the quota runs out OnQuotaHold must be called ONCE (even if three jobs
// get 509 at the same time); the UI turns it into a system notification.
func TestQuotaHoldNotifiesOnce(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin")
	f.setQuota(true)
	h := newHarness(t, f, "", 3)
	h.e.opt.QuotaProbeEvery = time.Hour
	got := make(chan string, 8)
	h.e.opt.OnQuotaHold = func(site string, at time.Time) {
		if at.IsZero() {
			t.Error("RetryAt arrived empty")
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
		t.Fatal("OnQuotaHold was not called")
	}
	select {
	case <-got:
		t.Error("OnQuotaHold was called a second time; it should be once every 10 min per site")
	case <-time.After(300 * time.Millisecond):
	}
}

// A failing command must not break the queue: the job stays waiting, the
// notification states the reason and the command's output, and the next
// quota can try again (after the cooldown).
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
	h.e.SetQuotaCommand("echo no vpn&& exit 7")
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
			if strings.Contains(n, "failed") {
				failure = n
			}
		case <-deadline:
			t.Fatal("no failure notification arrived")
		}
	}
	if !strings.Contains(failure, "no vpn") {
		t.Errorf("the command output is missing from the notification: %q", failure)
	}
	if got := jobByName(h.e, "a.bin").State; got != StateWaiting {
		t.Errorf("the failing command broke the job's state: %s", got)
	}
}

// The hold must stay in place when the app is closed and reopened: RetryAt is
// written to disk; if its time has passed the job returns to the queue on launch.
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
		t.Fatalf("reloaded job: %+v", jobs[0])
	}

	// Let the reset time be in the past: on launch it must go straight back to the queue and download.
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

// --- Cancel all ---

// cancelAllSetup builds a queue with one job in each interesting state: a
// finished one (a), a paused one with a partial file (b), a running one held
// mid-body (c) and a queued one (d). It returns the release channel of c.
func cancelAllSetup(t *testing.T, h *harness) chan struct{} {
	t.Helper()
	add := func(name string) {
		if _, err := h.e.Add(context.Background(), "file://"+name, h.out); err != nil {
			t.Fatal(err)
		}
	}
	add("a.bin")
	h.waitState(jobByName(h.e, "a.bin").ID, StateDone, 10*time.Second)

	holdB := h.f.holdNext("b.bin")
	add("b.bin")
	b := h.waitState(jobByName(h.e, "b.bin").ID, StateRunning, 5*time.Second)
	time.Sleep(300 * time.Millisecond) // the first 32 KB reaches the disk
	h.e.Pause(b.ID)
	close(holdB)
	h.waitState(b.ID, StatePaused, 5*time.Second)

	holdC := h.f.holdNext("c.bin")
	add("c.bin")
	h.waitState(jobByName(h.e, "c.bin").ID, StateRunning, 5*time.Second)
	time.Sleep(300 * time.Millisecond)

	add("d.bin") // MaxActive is 1: stays queued behind c
	return holdC
}

// "Cancel all": every unfinished job leaves the queue (the running one is
// stopped), the partial files go, the finished file and its row stay, and
// nothing starts afterwards.
func TestCancelAllRemovesUnfinishedAndWipesPartials(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin", "d.bin")
	h := newHarness(t, f, "", 1)
	h.start()
	defer h.stop()
	holdC := cancelAllSetup(t, h)

	if n := h.e.CancelAll(true); n != 3 {
		t.Fatalf("CancelAll canceled %d jobs, want 3 (paused, running, queued)", n)
	}
	close(holdC)

	jobs := h.e.Jobs()
	if len(jobs) != 1 || jobs[0].Filename != "a.bin" || jobs[0].State != StateDone {
		t.Fatalf("only the finished job should remain: %+v", states(jobs))
	}
	album := filepath.Join(h.out, "Album")
	deadline := time.Now().Add(5 * time.Second)
	for {
		left := []string{}
		for _, name := range []string{"b.bin", "c.bin"} {
			for _, suffix := range []string{".part", ".part.state"} {
				if _, err := os.Stat(filepath.Join(album, name+suffix)); err == nil {
					left = append(left, name+suffix)
				}
			}
		}
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("partial files remained after CancelAll(true): %v", left)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got, err := os.ReadFile(filepath.Join(album, "a.bin")); err != nil || !bytes.Equal(got, f.files["a.bin"]) {
		t.Fatalf("the finished file was touched: %v", err)
	}
	// A vanished .part could also mean the running job finished; it must not have.
	if _, err := os.Stat(filepath.Join(album, "c.bin")); err == nil {
		t.Error("the running job completed although it was canceled")
	}

	// Nothing may start afterwards: d was canceled before it ever ran.
	time.Sleep(300 * time.Millisecond)
	if n := f.hitCount("d.bin"); n != 0 {
		t.Errorf("a canceled queued job was downloaded (%d requests)", n)
	}
	if len(h.e.Jobs()) != 1 {
		t.Errorf("a canceled job came back: %+v", states(h.e.Jobs()))
	}
}

// CancelAll(false) empties the queue the same way but leaves the partial
// files: adding the same link later continues where it stopped.
func TestCancelAllCanKeepPartials(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin", "d.bin")
	h := newHarness(t, f, "", 1)
	h.start()
	defer h.stop()
	holdC := cancelAllSetup(t, h)

	if n := h.e.CancelAll(false); n != 3 {
		t.Fatalf("CancelAll canceled %d jobs, want 3", n)
	}
	close(holdC)
	h.waitActiveZero(5 * time.Second)

	for _, name := range []string{"b.bin", "c.bin"} {
		if _, err := os.Stat(filepath.Join(h.out, "Album", name+".part")); err != nil {
			t.Errorf("%s: the partial file was deleted although it should be kept: %v", name, err)
		}
	}
	// The same link again: b continues from its .part (a Range request).
	before := f.hitCount("b.bin")
	if _, err := h.e.Add(context.Background(), "file://b.bin", h.out); err != nil {
		t.Fatal(err)
	}
	h.waitState(jobByName(h.e, "b.bin").ID, StateDone, 10*time.Second)
	got, _ := os.ReadFile(filepath.Join(h.out, "Album", "b.bin"))
	if !bytes.Equal(got, f.files["b.bin"]) {
		t.Fatal("content corrupted after continuing a kept partial")
	}
	if f.hitCount("b.bin") != before+1 {
		t.Errorf("b was fetched %d more times, want 1 (a single resume)", f.hitCount("b.bin")-before)
	}
}

// A quota or captcha hold belonged to the canceled jobs; it must not keep the
// next job added for that site from starting.
func TestCancelAllLiftsHolds(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
	h := newHarness(t, f, "", 1)
	h.e.mu.Lock()
	h.e.captchaHold["fake"] = true
	h.e.netBackoffUntil["fake"] = time.Now().Add(time.Hour)
	h.e.mu.Unlock()
	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	h.e.CancelAll(true)
	if len(h.e.CaptchaHeld()) != 0 {
		t.Fatalf("the captcha hold survived CancelAll: %v", h.e.CaptchaHeld())
	}
	h.start()
	defer h.stop()
	if _, err := h.e.Add(context.Background(), "file://b.bin", h.out); err != nil {
		t.Fatal(err)
	}
	h.waitState(jobByName(h.e, "b.bin").ID, StateDone, 10*time.Second)
}

// waitActiveZero waits until no job goroutine is running.
func (h *harness) waitActiveZero(timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		h.e.mu.Lock()
		n := len(h.e.active)
		h.e.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatal("jobs are still running")
}

// --- Connections per file ---

// The user's choice must reach the download: with 4 requested and a site
// ceiling of 4 the job reports 4 connections while it runs, and 0 once it is
// done. This is the number the row shows.
func TestRunningJobReportsConnections(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	f.delay = 150 * time.Millisecond // every segment is in flight before data arrives
	cfg := site.SiteConfig{Name: "fake", MaxConcurrent: 8, MaxSegments: 4, MaxRetries: 2}.WithDefaults()
	var mu sync.Mutex
	maxConns := 0
	e, err := New(Options{
		MaxActive: 1,
		Client:    f.srv.Client(),
		Resolvers: []site.Resolver{&fakeResolver{f: f}},
		Configs:   []site.SiteConfig{cfg},
		OnChange: func(j Job) {
			mu.Lock()
			if j.Conns > maxConns {
				maxConns = j.Conns
			}
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.workers["fake"].Down.MinSegmentSize = 16 << 10 // the fake file is 96 KB
	e.SetSegments(8)                                 // the ceiling (4) applies
	h := &harness{t: t, f: f, e: e, out: testutil.TempDir(t)}
	h.start()
	defer h.stop()

	if _, err := e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	j := h.waitState(jobByName(e, "a.bin").ID, StateDone, 10*time.Second)
	got, _ := os.ReadFile(filepath.Join(h.out, "Album", "a.bin"))
	if !bytes.Equal(got, f.files["a.bin"]) {
		t.Fatal("segmented content is corrupt")
	}
	mu.Lock()
	defer mu.Unlock()
	if maxConns != 4 {
		t.Errorf("the job reported at most %d connections, want 4", maxConns)
	}
	if j.Conns != 0 {
		t.Errorf("a finished job still reports %d connections", j.Conns)
	}
}

// A job in a nested folder (mega, gofile and mediafire keep subfolders) gets
// its progress and connections while it runs. MEASURED: on Windows the
// downloader reported "Vids\Clips" for the job's "Vids/Clips", nothing
// matched, and every row sat at 0% with no speed, time left or connections.
func TestNestedFolderJobGetsProgress(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	f.delay = 150 * time.Millisecond
	cfg := site.SiteConfig{Name: "fake", MaxConcurrent: 8, MaxSegments: 4, MaxRetries: 2}.WithDefaults()
	var mu sync.Mutex
	var done int64
	var conns int
	e, err := New(Options{
		MaxActive: 1,
		Client:    f.srv.Client(),
		Resolvers: []site.Resolver{&fakeResolver{f: f, dir: "Album/Clips: 2024"}},
		Configs:   []site.SiteConfig{cfg},
		OnChange: func(j Job) {
			if j.State != StateRunning {
				return
			}
			mu.Lock()
			done = max(done, j.Done)
			conns = max(conns, j.Conns)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.workers["fake"].Down.MinSegmentSize = 16 << 10
	h := &harness{t: t, f: f, e: e, out: testutil.TempDir(t)}
	h.start()
	defer h.stop()

	if _, err := e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	h.waitState(jobByName(e, "a.bin").ID, StateDone, 10*time.Second)
	mu.Lock()
	defer mu.Unlock()
	if done == 0 || conns == 0 {
		t.Errorf("while running the job showed %d bytes over %d connections; want both above 0", done, conns)
	}
}

// SegmentCeilings lists every site's ceiling (for the UI's explanation).
func TestSegmentCeilings(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	e, err := New(Options{
		Client:    f.srv.Client(),
		Resolvers: []site.Resolver{&fakeResolver{f: f}, &fakeResolver{f: f, prefix: "b-"}},
		Configs: []site.SiteConfig{
			site.SiteConfig{Name: "zeta", MaxSegments: 3}.WithDefaults(),
			site.SiteConfig{Name: "alpha"}.WithDefaults(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := e.SegmentCeilings()
	if len(got) != 2 || got[0] != (SiteCeiling{"alpha", 1}) || got[1] != (SiteCeiling{"zeta", 3}) {
		t.Fatalf("SegmentCeilings = %+v", got)
	}
}

// --- Downloads at once ---

// maxRunning samples how many jobs run at once until every job is done.
func (h *harness) maxRunning(timeout time.Duration, during func(seen int)) int {
	h.t.Helper()
	maxSeen := 0
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n, done := 0, 0
		jobs := h.e.Jobs()
		for _, j := range jobs {
			switch j.State {
			case StateRunning:
				n++
			case StateDone:
				done++
			}
		}
		if n > maxSeen {
			maxSeen = n
		}
		if during != nil {
			during(n)
		}
		if done == len(jobs) && len(jobs) > 0 {
			return maxSeen
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("not every job finished: %+v", states(h.e.Jobs()))
	return maxSeen
}

// "Downloads at once" can change while the queue runs: raising it starts
// more jobs right away.
func TestSetMaxActiveTakesEffectLive(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin", "d.bin", "e.bin", "f.bin")
	f.delay = 200 * time.Millisecond
	h := newHarness(t, f, "", 1)
	h.start()
	defer h.stop()
	if got := h.e.MaxActive(); got != 1 {
		t.Fatalf("MaxActive = %d", got)
	}
	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	raised := false
	max := h.maxRunning(15*time.Second, func(n int) {
		if !raised && n == 1 {
			raised = true
			h.e.SetMaxActive(4)
		}
	})
	if max != 4 {
		t.Errorf("at most %d jobs ran at once after raising the limit to 4", max)
	}
}

// A site's max_concurrent caps its share even when more downloads are
// allowed at once: the rest stay "queued" rather than sitting inside the
// worker as "downloading" at 0%.
func TestSiteShareIsCappedByMaxConcurrent(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin", "d.bin")
	f.delay = 150 * time.Millisecond
	cfg := site.SiteConfig{Name: "fake", MaxConcurrent: 2, MaxRetries: 2}.WithDefaults()
	e, err := New(Options{
		MaxActive: 4,
		Client:    f.srv.Client(),
		Resolvers: []site.Resolver{&fakeResolver{f: f}},
		Configs:   []site.SiteConfig{cfg},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, f: f, e: e, out: testutil.TempDir(t)}
	h.start()
	defer h.stop()
	if _, err := e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	if max := h.maxRunning(15*time.Second, nil); max != 2 {
		t.Errorf("%d of the site's jobs ran at once; max_concurrent is 2", max)
	}
}

// --- Links handed to yt-dlp / gallery-dl ---

// A playlist page added to the queue becomes one job per video. After a
// restart the jobs go straight back to yt-dlp: the page isn't fetched again,
// and each row ends up named after the file yt-dlp wrote.
func TestToolJobsSurviveRestart(t *testing.T) {
	var pageHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pageHits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>a playlist page</html>")
	}))
	t.Cleanup(srv.Close)
	exe := testutil.FakeToolPath(t)
	cfg := site.SiteConfig{
		Name: site.DirectName, MaxConcurrent: 4, MaxRetries: 1, HTTPClient: srv.Client(),
		Extra: map[string]string{"yt_dlp": exe, "gallery_dl": exe, "ffmpeg": filepath.Join(t.TempDir(), "none")},
	}.WithDefaults()
	state := filepath.Join(testutil.TempDir(t), "queue.json")
	out := testutil.TempDir(t)
	newEngine := func() *Engine {
		e, err := New(Options{StatePath: state, Client: srv.Client(),
			Resolvers: []site.Resolver{site.NewDirect(cfg)}, Configs: []site.SiteConfig{cfg}})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}

	first := newEngine()
	if n, err := first.Add(context.Background(), srv.URL+"/playlist", out); err != nil || n != 2 {
		t.Fatalf("Add: %d %v", n, err)
	}
	first.flush()
	hitsAfterAdd := pageHits.Load()

	e := newEngine() // "restarted": nothing resolved in memory
	h := &harness{t: t, e: e, out: out}
	h.start()
	defer h.stop()
	h.waitAll(StateDone, 20*time.Second)
	for _, j := range e.Jobs() {
		if !strings.HasSuffix(j.Filename, ".mp4") || j.Dir != "My list" {
			t.Errorf("job %+v", j)
		}
		if _, err := os.Stat(j.Path); err != nil {
			t.Errorf("%s: %v", j.Filename, err)
		}
	}
	if pageHits.Load() != hitsAfterAdd {
		t.Errorf("the page was fetched again after the restart (%d requests)", pageHits.Load()-hitsAfterAdd)
	}
}
