package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
)

// TestMain sets up a headless Fyne app: some helpers touch Fyne and panic
// with a nil pointer without a running app.
func TestMain(m *testing.M) {
	test.NewApp()
	os.Exit(m.Run())
}

func job(id string, st queue.State, done, size int64) queue.Job {
	return queue.Job{ID: id, Filename: id + ".mp4", State: st, Done: done, Size: size}
}

// --- View model ---

func TestViewModelKeepsInsertionOrder(t *testing.T) {
	vm := newViewModel()
	vm.Apply(job("b", queue.StateQueued, 0, 10))
	vm.Apply(job("a", queue.StateQueued, 0, 10))
	vm.Apply(job("b", queue.StateRunning, 5, 10)) // an update must not break the order
	rows := vm.Rows()
	if len(rows) != 2 || rows[0].Job.ID != "b" || rows[1].Job.ID != "a" {
		t.Fatalf("order broken: %+v", rows)
	}
	if rows[0].Job.State != queue.StateRunning {
		t.Error("the update was not applied")
	}
}

// Speed is only measured for running jobs; a stopped job's speed is cleared.
func TestViewModelTracksRateOnlyWhileRunning(t *testing.T) {
	vm := newViewModel()
	now := time.Now()
	vm.mu.Lock()
	vm.apply(job("a", queue.StateRunning, 0, 1<<20), now)
	vm.apply(job("a", queue.StateRunning, 512<<10, 1<<20), now.Add(time.Second))
	vm.mu.Unlock()
	r, _ := vm.Row(0)
	if r.Rate <= 0 {
		t.Fatalf("the running job's speed was not measured: %v", r.Rate)
	}
	vm.Apply(job("a", queue.StatePaused, 512<<10, 1<<20))
	r, _ = vm.Row(0)
	if r.Rate != 0 {
		t.Fatalf("the paused job's speed remained: %v", r.Rate)
	}
}

// Replace syncs with the full list from the engine: a removed job must drop.
func TestViewModelReplaceDropsMissing(t *testing.T) {
	vm := newViewModel()
	vm.Apply(job("a", queue.StateQueued, 0, 10))
	vm.Apply(job("b", queue.StateQueued, 0, 10))
	vm.Replace([]queue.Job{job("b", queue.StateDone, 10, 10)})
	rows := vm.Rows()
	if len(rows) != 1 || rows[0].Job.ID != "b" || rows[0].Job.State != queue.StateDone {
		t.Fatalf("Replace wrong: %+v", rows)
	}
}

// The change flag: for periodic drawing, not per event.
func TestViewModelDirtyFlag(t *testing.T) {
	vm := newViewModel()
	if vm.TakeDirty() {
		t.Fatal("dirty on an empty model")
	}
	vm.Apply(job("a", queue.StateQueued, 0, 10))
	if !vm.TakeDirty() {
		t.Fatal("not dirty after a change")
	}
	if vm.TakeDirty() {
		t.Fatal("the flag was not reset")
	}
}

func TestSummaryCountsAndTotalRate(t *testing.T) {
	vm := newViewModel()
	if got := vm.Summary(); !strings.Contains(got, "empty") {
		t.Errorf("empty summary: %q", got)
	}
	now := time.Now()
	vm.mu.Lock()
	vm.apply(job("a", queue.StateRunning, 0, 1<<20), now)
	vm.apply(job("a", queue.StateRunning, 1<<20, 1<<20), now.Add(time.Second))
	vm.apply(job("b", queue.StateQueued, 0, 5), now)
	vm.apply(job("c", queue.StateDone, 5, 5), now)
	vm.apply(job("d", queue.StatePaused, 1, 5), now)
	vm.apply(job("e", queue.StateFailed, 0, 5), now)
	vm.mu.Unlock()
	got := vm.Summary()
	for _, want := range []string{"1 active", "1 queued", "1 paused", "1 failed", "1 done", "MB/s"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from the summary %q", want, got)
		}
	}
}

// A notice must not be overwritten by the refresh loop's summary: permanent
// with an empty queue, next to the summary with a non-empty queue, and giving
// way to the summary after 30 s.
func TestStatusLineKeepsNoticeOverSummary(t *testing.T) {
	vm := newViewModel()
	vm.TakeDirty()
	vm.Notify("Could not add: folder is empty")
	if !vm.TakeDirty() {
		t.Fatal("the notice did not ask for a redraw")
	}
	now := time.Now()
	if got := vm.statusLine(now.Add(time.Hour)); got != "Could not add: folder is empty" {
		t.Errorf("the notice was lost with an empty queue: %q", got)
	}
	vm.mu.Lock()
	vm.apply(job("a", queue.StateQueued, 0, 5), now)
	vm.mu.Unlock()
	if got := vm.statusLine(now); !strings.HasPrefix(got, "Could not add: folder is empty  ·  1 queued") {
		t.Errorf("notice + summary expected with a non-empty queue: %q", got)
	}
	if got := vm.statusLine(now.Add(noticeTTL + time.Second)); got != "1 queued" {
		t.Errorf("an expired notice must go away: %q", got)
	}
}

// A row waiting for quota: the time and the time left are visible, there's a
// ▶ hint; "shortly" when the wait is short; "try now" straight away without RetryAt.
func TestWaitingMetaShowsRetryClock(t *testing.T) {
	now := time.Date(2026, 9, 14, 15, 25, 0, 0, time.Local)
	j := queue.Job{State: queue.StateWaiting, RetryAt: now.Add(5*time.Hour + 6*time.Minute)}
	got := waitingMeta(j, now)
	for _, want := range []string{"20:31", "5h 6m", "resume to try now"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from %q", want, got)
		}
	}
	if got := waitingMeta(queue.Job{State: queue.StateWaiting, RetryAt: now.Add(20 * time.Second)}, now); !strings.Contains(got, "shortly") {
		t.Errorf("short wait: %q", got)
	}
	if got := waitingMeta(queue.Job{State: queue.StateWaiting}, now); !strings.Contains(got, "try now") {
		t.Errorf("without RetryAt: %q", got)
	}
	if act := actionFor(queue.StateWaiting); act != actionResume {
		t.Errorf("a waiting job can't be resumed: %v", act)
	}
}

// The notification body: what to do and what happens if you don't.
func TestQuotaHoldMessage(t *testing.T) {
	now := time.Date(2026, 9, 14, 15, 25, 0, 0, time.Local)
	got := quotaHoldMessage(now.Add(5*time.Hour+6*time.Minute), now)
	for _, want := range []string{"VPN", "by themselves", "20:31", "5h 6m"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from %q", want, got)
		}
	}
	if got := quotaHoldMessage(time.Time{}, now); !strings.Contains(got, "by themselves") || strings.Contains(got, "retried") {
		t.Errorf("message without a time: %q", got)
	}
}

// The banner only while jobs wait for quota; count, site and the earliest time.
func TestQuotaBanner(t *testing.T) {
	vm := newViewModel()
	now := time.Date(2026, 9, 14, 15, 25, 0, 0, time.Local)
	if got := vm.quotaBanner(now); got != "" {
		t.Fatalf("banner with an empty queue: %q", got)
	}
	vm.mu.Lock()
	a := job("a", queue.StateWaiting, 0, 5)
	a.Site, a.RetryAt = "mega", now.Add(2*time.Hour)
	b := job("b", queue.StateWaiting, 0, 5)
	b.Site, b.RetryAt = "mega", now.Add(time.Hour)
	vm.apply(a, now)
	vm.apply(b, now)
	vm.apply(job("c", queue.StateRunning, 1, 5), now)
	vm.mu.Unlock()
	got := vm.quotaBanner(now)
	for _, want := range []string{"mega", "2 files", "VPN", "16:25", "1h"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from %q", want, got)
		}
	}
	vm.mu.Lock()
	a.State, b.State = queue.StateDone, queue.StateDone
	vm.apply(a, now)
	vm.apply(b, now)
	vm.mu.Unlock()
	if got := vm.quotaBanner(now); got != "" {
		t.Errorf("the banner did not go away once nothing was waiting: %q", got)
	}
}

// --- Row formatting ---

func TestCellTextsByState(t *testing.T) {
	now := time.Now()
	run := row{Job: job("a", queue.StateRunning, 20<<20, 100<<20), Rate: 10 << 20}
	if speedText(run) == "" || etaText(run) == "" || sizeText(run.Job) != "100.0 MB" {
		t.Errorf("running row: size %q, speed %q, eta %q", sizeText(run.Job), speedText(run), etaText(run))
	}
	// With an unknown size the time left must NOT BE MADE UP; the size
	// column shows what has come so far.
	unk := row{Job: job("u", queue.StateRunning, 5<<20, -1), Rate: 1 << 20}
	if got := etaText(unk); got != "" {
		t.Errorf("time left on a row without a size: %q", got)
	}
	if got := sizeText(unk.Job); got != "5.0 MB" {
		t.Errorf("size of a row without a size: %q", got)
	}
	// With an unknown speed "0 B/s" must not be written.
	slow := row{Job: job("s", queue.StateRunning, 1, 100), Rate: 0}
	if got := speedText(slow); strings.Contains(got, "/s") {
		t.Errorf("speed on a row without a speed: %q", got)
	}
	// Only a running job has a speed and a time left.
	paused := row{Job: job("p", queue.StatePaused, 20, 100), Rate: 5 << 20}
	if speedText(paused) != "" || etaText(paused) != "" {
		t.Errorf("paused row: speed %q, eta %q", speedText(paused), etaText(paused))
	}
	failed := job("f", queue.StateFailed, 0, 10)
	failed.Error = "mega transfer quota exceeded (HTTP 509): per-IP limit\nsecond line"
	if got := statusText(row{Job: failed}, now); !strings.HasPrefix(got, "Failed: ") || !strings.Contains(got, "quota") || strings.Contains(got, "second") {
		t.Errorf("a failed row must show the first line: %q", got)
	}
	if got := statusText(row{Job: job("d", queue.StateDone, 10, 10)}, now); got != "Done" {
		t.Errorf("finished row: %q", got)
	}
	if got := statusText(row{Job: job("k", queue.StateSkipped, 10, 10)}, now); got != "Already downloaded" {
		t.Errorf("skipped row: %q", got)
	}
}

func TestRowProgress(t *testing.T) {
	if p := rowProgress(job("a", queue.StateRunning, 50, 200)); p < 0.24 || p > 0.26 {
		t.Errorf("progress = %v", p)
	}
	if p := rowProgress(job("a", queue.StateRunning, 50, -1)); p != 0 {
		t.Errorf("progress made up without a size: %v", p)
	}
	if p := rowProgress(job("a", queue.StateDone, 0, 0)); p != 1 {
		t.Errorf("a finished job must look full: %v", p)
	}
	if p := rowProgress(job("a", queue.StateRunning, 300, 200)); p != 1 {
		t.Errorf("overflow must be clipped to 1: %v", p)
	}
}

func TestActionForState(t *testing.T) {
	cases := map[queue.State]rowAction{
		queue.StateQueued: actionPause, queue.StateRunning: actionPause,
		queue.StatePaused: actionResume, queue.StateFailed: actionResume, queue.StateStopped: actionResume,
		queue.StateDone: actionNone, queue.StateSkipped: actionNone,
	}
	for st, want := range cases {
		if got := actionFor(st); got != want {
			t.Errorf("%s -> %v, want %v", st, got, want)
		}
	}
}

// --- Speed limit input ---

func TestParseSpeedLimit(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"", 0, false}, {"0", 0, false}, {"2", 2 << 20, false},
		{"2.5", int64(2.5 * 1024 * 1024), false}, {"2,5", int64(2.5 * 1024 * 1024), false},
		{"abc", 0, true}, {"-1", 0, true},
	}
	for _, c := range cases {
		got, err := parseSpeedLimit(c.in)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("parseSpeedLimit(%q) = %d, %v; want %d, err=%v", c.in, got, err, c.want, c.err)
		}
	}
}

// --- Connections ---

// A running row shows the connections its download really got; nothing while unknown.
func TestStatusShowsConnections(t *testing.T) {
	now := time.Now()
	j := job("a", queue.StateRunning, 20<<20, 100<<20)
	j.Conns = 8
	if got := statusText(row{Job: j, Rate: 10 << 20}, now); got != "Downloading  ·  8 connections" {
		t.Errorf("running row with 8 connections: %q", got)
	}
	j.Conns = 1
	if got := statusText(row{Job: j}, now); !strings.Contains(got, "1 connection") || strings.Contains(got, "connections") {
		t.Errorf("running row with 1 connection: %q", got)
	}
	j.Conns = 0
	if got := statusText(row{Job: j}, now); got != "Downloading" {
		t.Errorf("a connection count was made up: %q", got)
	}
}

// The notice after picking a number names only the sites that cap it.
func TestSegmentsNotice(t *testing.T) {
	ceilings := []queue.SiteCeiling{{Site: "bunkr", Max: 3}, {Site: "mega", Max: 8}, {Site: "pixeldrain", Max: 1}}
	got := segmentsNotice(8, ceilings)
	for _, want := range []string{"8", "3 on bunkr", "1 on pixeldrain"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from %q", want, got)
		}
	}
	if strings.Contains(got, "on mega") {
		t.Errorf("mega doesn't cap 8 but is named: %q", got)
	}
	if got := segmentsNotice(1, ceilings); strings.Contains(got, "limit") {
		t.Errorf("nothing caps 1, yet: %q", got)
	}
}

// "Cancel all" counts only unfinished jobs and their downloaded bytes.
func TestUnfinishedCountsForCancelAll(t *testing.T) {
	vm := newViewModel()
	vm.Apply(job("a", queue.StateDone, 10, 10))
	vm.Apply(job("b", queue.StatePaused, 4, 10))
	vm.Apply(job("c", queue.StateRunning, 3, 10))
	vm.Apply(job("d", queue.StateQueued, 0, 10))
	vm.Apply(job("e", queue.StateSkipped, 10, 10))
	if n, partial := vm.Unfinished(); n != 3 || partial != 7 {
		t.Errorf("Unfinished = %d jobs, %d bytes; want 3, 7", n, partial)
	}
}
