package queue

// The user's commands: pause, resume, remove, cancel all, retry failed, and
// the settings that apply to the whole queue.

import (
	"os"
	"sort"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// ---------- Controls ----------

// Pause stops a job. If it is running it is canceled (the .part stays in
// place); if it is queued it is marked paused.
func (e *Engine) Pause(id string) {
	e.mu.Lock()
	j, ok := e.byID[id]
	if !ok {
		e.mu.Unlock()
		return
	}
	var snap *Job
	switch j.State {
	case StateRunning:
		e.pauseWant[id] = true
		if cancel := e.active[id]; cancel != nil {
			cancel()
		}
		// The state will be moved to Paused when the running goroutine ends.
	case StateQueued:
		j.State = StatePaused
		s := *j
		snap = &s
	}
	e.mu.Unlock()
	if snap != nil {
		e.changed(*snap)
		e.scheduleSave()
	}
}

// Resume puts a paused, failed or stopped job back in the queue.
func (e *Engine) Resume(id string) {
	e.mu.Lock()
	j, ok := e.byID[id]
	if !ok || !j.State.Resumable() {
		e.mu.Unlock()
		return
	}
	j.State = StateQueued
	j.Error = ""
	j.RetryAt = time.Time{}
	// The user explicitly said "resume". Lift the site's captcha hold;
	// otherwise the job shows "queued" but never starts and the user thinks
	// it is "blocked" (measured: exactly this happened after switching VPN
	// and pressing ▶). The network backoff is reset too: the user is saying
	// the network is back.
	delete(e.captchaHold, j.Site)
	delete(e.netFails, j.Site)
	delete(e.netBackoffUntil, j.Site)
	e.clearBusy(j.Site)
	snap := *j
	e.mu.Unlock()
	e.changed(snap)
	e.scheduleSave()
	e.kick()
}

// CaptchaHeld returns the sites (sorted) whose jobs are held because of a
// captcha; the UI shows the reason in the button label.
func (e *Engine) CaptchaHeld() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.captchaHold))
	for name := range e.captchaHold {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Remove takes a job out of the queue. If deleteFiles is true the partial
// file and its state are deleted too; a completed file is NEVER TOUCHED.
func (e *Engine) Remove(id string, deleteFiles bool) {
	e.mu.Lock()
	j, ok := e.byID[id]
	if !ok {
		e.mu.Unlock()
		return
	}
	running := false
	if cancel := e.active[id]; cancel != nil {
		// Running: deleting the file NOW would race (the goroutine is still
		// writing and may recreate the state file). Deletion happens in runJob
		// when the goroutine ends.
		e.pauseWant[id] = true
		e.wipeWant[id] = deleteFiles && !j.State.Finished()
		running = true
		cancel()
	}
	delete(e.byID, id)
	delete(e.items, id)
	for i, x := range e.jobs {
		if x.ID == id {
			e.jobs = append(e.jobs[:i], e.jobs[i+1:]...)
			break
		}
	}
	path := j.Path
	finished := j.State.Finished()
	e.mu.Unlock()

	if !running && deleteFiles && path != "" && !finished {
		wipePartial(path)
	}
	e.scheduleSave()
	e.kick()
}

// wipePartial deletes the partial file and its state; it doesn't touch a completed file.
func wipePartial(path string) {
	_ = os.Remove(path + ".part")
	_ = os.Remove(path + ".part.state")
	_ = os.Remove(path + ".part.stmp")
}

// CancelAll takes every unfinished job out of the queue: running ones are
// stopped; queued, paused, failed, stopped and quota-waiting ones are
// removed. Finished rows (done, already downloaded) stay, and a completed
// file is NEVER TOUCHED. If deleteFiles is true the partial files are
// deleted too; a running job's only once its goroutine has stopped writing,
// exactly as in Remove. Returns the number of jobs canceled.
//
// The captcha, quota and network holds go as well: they belonged to the jobs
// that are gone. Left behind, they would silently keep the next jobs added
// for that site from starting. The user's own "Pause all" is kept.
func (e *Engine) CancelAll(deleteFiles bool) int {
	e.mu.Lock()
	var wipe []string
	kept := make([]*Job, 0, len(e.jobs))
	n := 0
	for _, j := range e.jobs {
		if j.State.Finished() {
			kept = append(kept, j)
			continue
		}
		n++
		if cancel := e.active[j.ID]; cancel != nil {
			e.pauseWant[j.ID] = true
			e.wipeWant[j.ID] = deleteFiles
			cancel()
		} else if deleteFiles && j.Path != "" {
			wipe = append(wipe, j.Path)
		}
		delete(e.byID, j.ID)
		delete(e.items, j.ID)
	}
	e.jobs = kept
	e.captchaHold = map[string]bool{}
	e.holdSince = map[string]time.Time{}
	e.nextProbe = map[string]time.Time{}
	e.probeItem = map[string]site.Item{}
	e.netBackoffUntil = map[string]time.Time{}
	e.netFails = map[string]int{}
	e.busyUntil = map[string]time.Time{}
	e.busyFails = map[string]int{}
	e.mu.Unlock()

	for _, p := range wipe {
		wipePartial(p)
	}
	if n > 0 {
		e.scheduleSave()
		e.kick()
	}
	return n
}

// PauseAll stops starting new jobs and pauses the running ones.
func (e *Engine) PauseAll() {
	e.mu.Lock()
	e.pausedAll = true
	ids := make([]string, 0, len(e.active))
	for id := range e.active {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	for _, id := range ids {
		e.Pause(id)
	}
}

// ResumeAll puts every paused job back in the queue, lifts the captcha holds
// and turns starting back on.
func (e *Engine) ResumeAll() {
	e.mu.Lock()
	e.pausedAll = false
	e.captchaHold = map[string]bool{}
	var snaps []Job
	for _, j := range e.jobs {
		if j.State == StatePaused || j.State == StateWaiting {
			j.State = StateQueued
			j.RetryAt = time.Time{}
			j.Error = ""
			snaps = append(snaps, *j)
		}
	}
	e.mu.Unlock()
	for _, s := range snaps {
		e.changed(s)
	}
	e.scheduleSave()
	e.kick()
}

// ClearFinished removes finished and skipped jobs from the list.
func (e *Engine) ClearFinished() {
	e.mu.Lock()
	kept := e.jobs[:0]
	for _, j := range e.jobs {
		if j.State.Finished() {
			delete(e.byID, j.ID)
			delete(e.items, j.ID)
			continue
		}
		kept = append(kept, j)
	}
	e.jobs = kept
	e.mu.Unlock()
	e.scheduleSave()
}

// SetSegments sets the requested connections per file. The effective value
// can't exceed the site ceiling (max_segments); 1 turns segmented download
// off. Running downloads aren't affected, the next ones to start use the new
// value.
func (e *Engine) SetSegments(n int) {
	if n < 1 {
		n = 1
	}
	e.mu.Lock()
	e.segments = n
	e.mu.Unlock()
	for _, w := range e.workers {
		eff := n
		if w.Cfg.MaxSegments > 0 && eff > w.Cfg.MaxSegments {
			eff = w.Cfg.MaxSegments
		}
		// Through the setter: downloads that are running read the value.
		w.Down.SetSegments(eff)
	}
}

// SegmentCeilings returns each site's ceiling on connections per file
// (max_segments), sorted by site name. The UI uses it to say why the number
// the user picked isn't reached on every site.
func (e *Engine) SegmentCeilings() []SiteCeiling {
	out := make([]SiteCeiling, 0, len(e.workers))
	for name, w := range e.workers {
		max := w.Cfg.MaxSegments
		if max < 1 {
			max = 1
		}
		out = append(out, SiteCeiling{Site: name, Max: max})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Site < out[b].Site })
	return out
}

// SiteCeiling is one site's connections-per-file ceiling.
type SiteCeiling struct {
	Site string
	Max  int
}

// Segments is the requested number of connections (before site ceilings apply).
func (e *Engine) Segments() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.segments
}

// SetMaxActive sets how many jobs are downloaded at the same time (at least
// 1). Lowering it doesn't stop running jobs; no new one starts until fewer
// than n run.
func (e *Engine) SetMaxActive(n int) {
	if n < 1 {
		n = 1
	}
	e.mu.Lock()
	e.opt.MaxActive = n
	e.mu.Unlock()
	e.kick()
}

// MaxActive is how many jobs are downloaded at the same time.
func (e *Engine) MaxActive() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.opt.MaxActive
}

// SetSpeedLimit sets the total bytes-per-second limit; 0 removes it.
func (e *Engine) SetSpeedLimit(bytesPerSec int64) { e.throttle.SetRate(bytesPerSec) }

// SpeedLimit returns the current limit.
func (e *Engine) SpeedLimit() int64 { return e.throttle.Rate() }

// Paused reports whether the global pause is on.
func (e *Engine) Paused() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pausedAll
}

// Jobs returns a snapshot of the queue in insertion order.
func (e *Engine) Jobs() []Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Job, len(e.jobs))
	for i, j := range e.jobs {
		out[i] = *j
	}
	return out
}

// RetryFailed puts every job in the "failed" state back in the queue. The
// user's explicit request: the network is back, the cause is fixed. The
// sites' network backoff is reset too; if the hold continued, the "retry"
// button would look like it does nothing.
func (e *Engine) RetryFailed() {
	e.mu.Lock()
	var snaps []Job
	for _, j := range e.jobs {
		if j.State == StateFailed {
			j.State = StateQueued
			j.Error = ""
			delete(e.netFails, j.Site)
			delete(e.netBackoffUntil, j.Site)
			e.clearBusy(j.Site)
			snaps = append(snaps, *j)
		}
	}
	e.mu.Unlock()
	for _, s := range snaps {
		e.changed(s)
	}
	if len(snaps) > 0 {
		e.scheduleSave()
		e.kick()
	}
}
