package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// viewModel is the queue as it is shown on screen. The engine's events flow
// in here; the UI reads from here. One direction between them: the model
// doesn't know about the UI.
//
// Why separate: Fyne widgets are tedious objects to test. Decisions such as
// row texts, percentages, speed and summary live here in pure functions and
// are tested without a window.
type viewModel struct {
	mu    sync.Mutex
	order []string
	jobs  map[string]queue.Job
	speed map[string]*speedo
	rate  map[string]float64
	dirty bool

	// notice is the result of an action ("Could not add: …", "Could not open
	// the folder: …"). It isn't written straight to the status line;
	// StatusLine combines it with the summary.
	notice   string
	noticeAt time.Time
}

// noticeTTL: with a non-empty queue the notice gives way to the summary
// after this long. With an empty queue there's nothing else to show, so the
// notice stays.
const noticeTTL = 30 * time.Second

func newViewModel() *viewModel {
	return &viewModel{
		jobs:  map[string]queue.Job{},
		speed: map[string]*speedo{},
		rate:  map[string]float64{},
	}
}

// Apply processes a single job update coming from the engine.
func (vm *viewModel) Apply(j queue.Job) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.apply(j, time.Now())
}

func (vm *viewModel) apply(j queue.Job, now time.Time) {
	if _, known := vm.jobs[j.ID]; !known {
		vm.order = append(vm.order, j.ID)
	}
	vm.jobs[j.ID] = j
	switch j.State {
	case queue.StateRunning:
		sp := vm.speed[j.ID]
		if sp == nil {
			sp = &speedo{}
			vm.speed[j.ID] = sp
		}
		vm.rate[j.ID] = sp.update(j.Done, now)
	default:
		// A stopped job has no speed; rather than writing "0 B/s" nothing is written.
		delete(vm.speed, j.ID)
		delete(vm.rate, j.ID)
	}
	vm.dirty = true
}

// Replace syncs the list with the engine's full copy. Removing and clearing
// produce no events, so it is called after them.
func (vm *viewModel) Replace(jobs []queue.Job) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	seen := map[string]bool{}
	vm.order = vm.order[:0]
	for _, j := range jobs {
		seen[j.ID] = true
		vm.order = append(vm.order, j.ID)
		if old, ok := vm.jobs[j.ID]; ok && old.State == queue.StateRunning && j.State == queue.StateRunning {
			// Keep the speed measurement; only refresh the data.
			vm.jobs[j.ID] = j
			continue
		}
		vm.apply(j, time.Now())
	}
	for id := range vm.jobs {
		if !seen[id] {
			delete(vm.jobs, id)
			delete(vm.speed, id)
			delete(vm.rate, id)
		}
	}
	vm.dirty = true
}

// TakeDirty reads and resets the "changed" flag. The UI asks for it
// periodically and only draws when something changed; drawing per event
// meant hundreds of refreshes per second with 20 parallel downloads.
func (vm *viewModel) TakeDirty() bool {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	d := vm.dirty
	vm.dirty = false
	return d
}

// row is what a single row needs for drawing.
type row struct {
	Job  queue.Job
	Rate float64
}

func (vm *viewModel) Len() int {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return len(vm.order)
}

func (vm *viewModel) Row(i int) (row, bool) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if i < 0 || i >= len(vm.order) {
		return row{}, false
	}
	id := vm.order[i]
	return row{Job: vm.jobs[id], Rate: vm.rate[id]}, true
}

// QuotaBanner is the text of the warning banner above the list; "" if no job
// waits for quota. It is the one place that doesn't depend on notifications
// and catches the eye when the window opens.
func (vm *viewModel) QuotaBanner() string {
	return vm.quotaBanner(time.Now())
}

func (vm *viewModel) quotaBanner(now time.Time) string {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	var n int
	var siteName string
	var earliest time.Time
	for _, id := range vm.order {
		j := vm.jobs[id]
		if j.State != queue.StateWaiting {
			continue
		}
		n++
		siteName = j.Site
		if !j.RetryAt.IsZero() && (earliest.IsZero() || j.RetryAt.Before(earliest)) {
			earliest = j.RetryAt
		}
	}
	if n == 0 {
		return ""
	}
	when := ""
	if !earliest.IsZero() && earliest.After(now) {
		when = fmt.Sprintf("; if you don't, they'll be retried automatically at %s (in %s)",
			earliest.Local().Format("15:04"), site.FormatWait(earliest.Sub(now)))
	}
	return fmt.Sprintf("%s quota exceeded — %d files waiting. Switch your VPN location; downloads resume by themselves once it changes%s.",
		siteName, n, when)
}

func (vm *viewModel) Rows() []row {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	out := make([]row, 0, len(vm.order))
	for _, id := range vm.order {
		out = append(out, row{Job: vm.jobs[id], Rate: vm.rate[id]})
	}
	return out
}

// Notify puts an action's result on the status line and asks for a redraw.
//
// MEASURED: when an action wrote "Could not add: …" and changed the model,
// the refresh loop overwrote the line with the summary ("Queue is empty…")
// 150 ms later; the user never got to see why the mega folder wasn't added.
// The notice now lives in the model and is rewritten on every draw.
func (vm *viewModel) Notify(msg string) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.notice = msg
	vm.noticeAt = time.Now()
	vm.dirty = true
}

// StatusLine is the whole bottom status line: the notice (if any and fresh) and the summary.
func (vm *viewModel) StatusLine() string {
	return vm.statusLine(time.Now())
}

func (vm *viewModel) statusLine(now time.Time) string {
	summary := vm.Summary()
	vm.mu.Lock()
	notice, at, empty := vm.notice, vm.noticeAt, len(vm.order) == 0
	vm.mu.Unlock()
	if notice == "" {
		return summary
	}
	if empty {
		return notice
	}
	if now.Sub(at) > noticeTTL {
		return summary
	}
	return notice + "  ·  " + summary
}

// Summary is the queue's summary: "2 active · 5 queued · 12 done · 24.3 MB/s".
func (vm *viewModel) Summary() string {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if len(vm.order) == 0 {
		return "The queue is empty. Paste links and press Add."
	}
	var active, queued, waiting, paused, done, failed int
	var total float64
	for _, id := range vm.order {
		j := vm.jobs[id]
		switch j.State {
		case queue.StateRunning:
			active++
			total += vm.rate[id]
		case queue.StateQueued:
			queued++
		case queue.StatePaused, queue.StateStopped:
			paused++
		case queue.StateWaiting:
			waiting++
		case queue.StateDone, queue.StateSkipped:
			done++
		case queue.StateFailed:
			failed++
		}
	}
	var parts []string
	if active > 0 {
		parts = append(parts, fmt.Sprintf("%d active", active))
	}
	if queued > 0 {
		parts = append(parts, fmt.Sprintf("%d queued", queued))
	}
	if waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d waiting for quota", waiting))
	}
	if paused > 0 {
		parts = append(parts, fmt.Sprintf("%d paused", paused))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	if done > 0 {
		parts = append(parts, fmt.Sprintf("%d done", done))
	}
	if r := humanRate(total); r != "" {
		parts = append(parts, r)
	}
	return strings.Join(parts, "  ·  ")
}

// --- Row formatting (pure) ---

// stateLabel is the display label of a state.
func stateLabel(s queue.State) string {
	switch s {
	case queue.StateQueued:
		return "queued"
	case queue.StateRunning:
		return "downloading"
	case queue.StatePaused:
		return "paused"
	case queue.StateDone:
		return "done"
	case queue.StateFailed:
		return "failed"
	case queue.StateSkipped:
		return "already downloaded"
	case queue.StateStopped:
		return "stopped"
	case queue.StateWaiting:
		return "waiting for quota"
	}
	return string(s)
}

// waitingMeta is the row of a job waiting for quota: when it will be retried
// by itself and what the user can do. The time is written ABSOLUTE ("at
// 20:31"): the list is only drawn on changes, so a countdown would stay frozen.
func waitingMeta(j queue.Job, now time.Time) string {
	if j.RetryAt.IsZero() {
		return "quota exceeded  ·  resume to try now"
	}
	left := j.RetryAt.Sub(now)
	if left < time.Minute {
		return "quota exceeded  ·  retrying shortly"
	}
	return fmt.Sprintf("quota exceeded  ·  resumes when the VPN changes or at %s (%s)  ·  resume to try now",
		j.RetryAt.Local().Format("15:04"), site.FormatWait(left))
}

// quotaHoldMessage is the body of the quota notification: what happened, what can be done.
func quotaHoldMessage(retryAt, now time.Time) string {
	if retryAt.IsZero() || retryAt.Before(now) {
		return "If you switch VPN server, downloads resume by themselves."
	}
	return fmt.Sprintf("If you switch VPN server, downloads resume by themselves; otherwise they'll be retried at %s (in %s).",
		retryAt.Local().Format("15:04"), site.FormatWait(retryAt.Sub(now)))
}

// connsLabel is "1 connection" / "8 connections"; empty while unknown.
func connsLabel(n int) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "1 connection"
	default:
		return fmt.Sprintf("%d connections", n)
	}
}

// segmentsNotice explains a new "Connections/file" choice: the number is a
// request, and sites whose ceiling is lower are named so the user isn't left
// wondering why a row shows fewer.
func segmentsNotice(n int, ceilings []queue.SiteCeiling) string {
	var capped []string
	for _, c := range ceilings {
		if c.Max < n {
			capped = append(capped, fmt.Sprintf("%d on %s", c.Max, c.Site))
		}
	}
	if len(capped) == 0 {
		return fmt.Sprintf("Connections per file: %d.", n)
	}
	return fmt.Sprintf("Connections per file: %d (site limits: %s). Each row shows what its download really gets.",
		n, strings.Join(capped, ", "))
}

// Unfinished counts the jobs "Cancel all" would take out of the queue, and
// the bytes already downloaded for them.
func (vm *viewModel) Unfinished() (n int, partial int64) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	for _, id := range vm.order {
		if j := vm.jobs[id]; !j.State.Finished() {
			n++
			partial += j.Done
		}
	}
	return n, partial
}

// rowProgress is the progress between 0 and 1; 0 if the size is unknown (the
// bar doesn't make things up).
func rowProgress(j queue.Job) float64 {
	switch j.State {
	case queue.StateDone, queue.StateSkipped:
		return 1
	}
	if j.Size <= 0 {
		return 0
	}
	p := float64(j.Done) / float64(j.Size)
	if p < 0 {
		return 0
	}
	if p > 1 {
		return 1
	}
	return p
}

// rowAction is what can be done to a job: the toolbar's and the row menu's
// Resume and Pause act on the jobs whose action matches.
type rowAction int

const (
	actionNone rowAction = iota
	actionPause
	actionResume
)

func actionFor(s queue.State) rowAction {
	switch s {
	case queue.StateRunning, queue.StateQueued:
		return actionPause
	case queue.StatePaused, queue.StateFailed, queue.StateStopped, queue.StateWaiting:
		return actionResume
	}
	return actionNone
}
