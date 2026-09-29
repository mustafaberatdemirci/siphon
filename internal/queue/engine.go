package queue

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/dl"
	"github.com/mustafaberatdemirci/siphon/internal/hook"
	snet "github.com/mustafaberatdemirci/siphon/internal/net"
	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/store"
)

// DefaultSegments is the requested connections per file if the user didn't
// set it. The site ceiling (max_segments) clips it: 1 on pixeldrain, 3 on
// bunkr, 8 on mega.
const DefaultSegments = 4

// DefaultMaxActive is the number of jobs downloaded at the same time, across
// sites (MegaBasterd's default is 4 too); the user changes it with
// SetMaxActive. A site's own max_concurrent still caps that site's share.
const DefaultMaxActive = 4

// saveDebounce is how often at most the queue file is written. Progress
// notifications arrive every 250 ms; writing to disk on each one is pointless.
const saveDebounce = 500 * time.Millisecond

// Options configures the engine.
type Options struct {
	ConfigPath string
	// StatePath is the queue file; if empty there is no persistence (tests).
	StatePath string
	MaxActive int
	Client    *http.Client
	// Events: Debugf/Errorf/Infof are used for logging. Item events are
	// handled by the engine; fields like ItemDone here are IGNORED.
	Events run.Events
	// OnChange is called whenever a job's state or progress changes. It
	// comes from background goroutines; the UI must move it to its own thread.
	OnChange func(Job)
	// OnNotice receives one-line events worth showing the user: the quota
	// command ran/failed, the allowance opened up. Optional.
	OnNotice func(string)
	// OnQuotaHold is called when a site's quota runs out and jobs are put on
	// hold (at most every 10 min per site). The UI turns it into a system
	// notification: the user gets the "switch the VPN" news even when not
	// looking at the app. Optional.
	OnQuotaHold func(siteName string, retryAt time.Time)

	// QuotaProbeEvery is how often a site whose quota ran out is checked for
	// allowance again; 0 means DefaultQuotaProbeEvery.
	QuotaProbeEvery time.Duration

	// NetRetryBase is the first wait for a site that couldn't be resolved
	// because of a network error (doubled on each consecutive error); 0
	// means DefaultNetRetryBase.
	NetRetryBase time.Duration

	// For tests: if given, the config isn't loaded and these are used.
	Resolvers []site.Resolver
	Configs   []site.SiteConfig
}

// DefaultQuotaProbeEvery: while a site's quota is exhausted, the first byte
// of one of its waiting files is requested at this interval. As soon as a
// byte comes back the quota has opened (the time passed or the user switched
// VPN) and EVERY waiting job of the site goes back into the queue at once.
//
// MegaBasterd gets the same effect by having every connection retry its
// chunk at most 8 s apart, forever (EXP_BACKOFF_MAX_WAIT_TIME); by default
// that is 24 connections knocking every 8 s. One one-byte request per site
// every 10 s notices a VPN switch just as quickly. The interval doesn't
// grow: the user may switch the VPN at any time, and a quota answer costs no
// bandwidth.
//
// Why try instead of asking the API: the tar field of the "uq" response came
// back as 0 both with the quota full and empty (measured); there is no
// reliable field to interpret.
const DefaultQuotaProbeEvery = 10 * time.Second

// QuotaCommandCooldown: the quota command runs at most this often. When
// three jobs get 509 at the same time the VPN must not switch three times
// (MegaBasterd: 120 s).
const QuotaCommandCooldown = 2 * time.Minute

// quotaCommandProbeDelay: after the command finishes the probe happens this
// much later; a short margin for the VPN tunnel to settle.
const quotaCommandProbeDelay = 3 * time.Second

// Backoff on a network outage. When the app opens before the network is up
// (or while the VPN switches), resolution gives DNS/connection errors. The
// job used to fall into "failed" right away and the next one started and fell
// too: within seconds the whole queue was "failed". Now the job stays in the
// queue and the site is held for a while: 15 s, 30 s, 1 min, 2 min, 4 min,
// 5 min (~13 min in total). If the network doesn't come back in that time the
// job falls into "failed" as before; showing "queued" forever would be a lie too.
const (
	DefaultNetRetryBase = 15 * time.Second
	maxNetRetryWait     = 5 * time.Minute
	maxNetFails         = 6
)

// quotaHoldNotifyEvery: OnQuotaHold at most this often for the same site. If
// the probe says "there is allowance" and the CDN gives 509 again the hold is
// set up again; notifications must not rain down each time.
const quotaHoldNotifyEvery = 10 * time.Minute

// Engine is the queue itself.
type Engine struct {
	opt      Options
	client   *http.Client
	throttle *dl.Throttle

	cfgs      []site.SiteConfig
	resolvers []site.Resolver
	workers   map[string]*run.Worker // site name -> worker

	mu        sync.Mutex
	jobs      []*Job
	byID      map[string]*Job
	items     map[string]site.Item          // runtime: resolved Item (gone after a restart)
	active    map[string]context.CancelFunc // running jobs
	pauseWant map[string]bool               // was the canceled job canceled at the user's request?
	wipeWant  map[string]bool               // should the removed job's partial file be deleted when it ends?
	ledgers   map[string]*store.Ledger      // outDir -> ledger
	pausedAll bool
	// captchaHold: sites asking for a captcha; that site's jobs aren't
	// started. A captcha is SITE-specific (pixeldrain's counter): a global
	// pause used to be switched on and mega/bunkr jobs stopped for nothing.
	// When the user says "resume" on one of that site's jobs the hold lifts
	// (the intent is clear: switched VPN, trying again); the user's own
	// "Pause all" is separate and kept.
	captchaHold map[string]bool
	closing     bool
	segments    int // connections per file the user asked for
	// holdSince: site -> the moment the quota ran out. When a job finishes
	// successfully it answers "did this job start AFTER the quota?" to release
	// the site's waiting jobs; a transfer that started before the quota and
	// was only allowed to finish doesn't prove the quota opened.
	holdSince map[string]time.Time
	// Quota probing (probeQuota):
	probing   map[string]bool      // a probe of the site is underway
	nextProbe map[string]time.Time // when the site's next probe happens
	// probeItem: the resolved item the site's probe asks about. Kept between
	// probes so every probe isn't an API call too; mega's URL is bound to the
	// IP, so after a VPN switch it answers 403 and is resolved again.
	probeItem map[string]site.Item
	// openNotified: when "allowance available again" was last said, per site.
	openNotified map[string]time.Time
	// Quota command (like the user's VPN-switching script):
	quotaCmd        string
	quotaCmdRunning bool
	quotaCmdLast    time.Time
	runCtx          context.Context // Run's context; the command and probes are tied to it
	quotaNotified   map[string]time.Time
	// Network outage: site -> jobs aren't started until this moment / consecutive error count.
	netBackoffUntil map[string]time.Time
	netFails        map[string]int

	wake chan struct{}

	saveMu    sync.Mutex
	saveTimer *time.Timer
}

// New loads the config, the resolvers and, if present, the queue file.
func New(opt Options) (*Engine, error) {
	if opt.MaxActive <= 0 {
		opt.MaxActive = DefaultMaxActive
	}
	// Log hooks are optional; they're filled with no-ops once instead of a
	// nil check on every call inside the engine.
	noop := func(string, ...any) {}
	if opt.Events.Debugf == nil {
		opt.Events.Debugf = noop
	}
	if opt.Events.Infof == nil {
		opt.Events.Infof = noop
	}
	if opt.Events.Errorf == nil {
		opt.Events.Errorf = noop
	}
	e := &Engine{
		opt:       opt,
		client:    opt.Client,
		throttle:  &dl.Throttle{},
		workers:   map[string]*run.Worker{},
		byID:      map[string]*Job{},
		items:     map[string]site.Item{},
		active:    map[string]context.CancelFunc{},
		pauseWant: map[string]bool{},
		wipeWant:  map[string]bool{},
		ledgers:   map[string]*store.Ledger{},
		wake:      make(chan struct{}, 1),

		holdSince:    map[string]time.Time{},
		probing:      map[string]bool{},
		captchaHold:  map[string]bool{},
		nextProbe:    map[string]time.Time{},
		probeItem:    map[string]site.Item{},
		openNotified: map[string]time.Time{},

		quotaNotified:   map[string]time.Time{},
		netBackoffUntil: map[string]time.Time{},
		netFails:        map[string]int{},
	}
	if e.opt.QuotaProbeEvery <= 0 {
		e.opt.QuotaProbeEvery = DefaultQuotaProbeEvery
	}
	if e.opt.NetRetryBase <= 0 {
		e.opt.NetRetryBase = DefaultNetRetryBase
	}
	if e.client == nil {
		e.client = snet.NewClient()
	}

	if len(opt.Resolvers) > 0 {
		e.cfgs, e.resolvers = opt.Configs, opt.Resolvers
	} else {
		cfgs, resolvers, err := run.Setup(opt.Events, opt.ConfigPath, nil, nil, e.client)
		if err != nil {
			return nil, err
		}
		e.cfgs, e.resolvers = cfgs, resolvers
	}
	for i, r := range e.resolvers {
		cfg := e.cfgs[i]
		ev := opt.Events
		ev.Progress = e.onProgress
		ev.Connections = e.onConnections
		w := run.NewWorker(r, cfg, e.client, ev)
		w.Down.Throttle = e.throttle
		e.workers[cfg.Name] = w
	}

	// The default request is 4 connections per file; site ceilings clip it.
	e.SetSegments(DefaultSegments)

	if opt.StatePath != "" {
		jobs, err := load(opt.StatePath)
		if err != nil {
			// A corrupt queue file must not keep the app from opening; start
			// empty, but say why.
			opt.Events.Errorf("%v — the queue starts empty", err)
		}
		for _, j := range jobs {
			e.jobs = append(e.jobs, j)
			e.byID[j.ID] = j
		}
	}
	return e, nil
}

// ---------- Adding to the queue ----------

// Add resolves a URL and adds its items to the queue. Resolution needs the
// network and can take seconds; the UI must call this from a separate
// goroutine. Adding the same file to the same folder a second time doesn't
// duplicate it. Returns the number of jobs added.
func (e *Engine) Add(ctx context.Context, rawURL, outDir string) (int, error) {
	r, idx := run.Pick(e.resolvers, rawURL)
	if r == nil {
		return 0, fmt.Errorf("no matching site: %s", rawURL)
	}
	siteName := e.cfgs[idx].Name

	added := 0
	itemErrs, err := r.Resolve(ctx, rawURL, func(it site.Item) error {
		j := &Job{
			ID:         jobID(outDir, it.SourcePage, it.Dir, it.Filename),
			Site:       siteName,
			SourcePage: it.SourcePage,
			OutDir:     outDir,
			Dir:        it.Dir,
			Filename:   it.Filename,
			Size:       it.Size,
			Index:      it.Index,
			State:      StateQueued,
			AddedAt:    time.Now(),
		}
		e.mu.Lock()
		if existing, dup := e.byID[j.ID]; dup {
			// If a finished job is added again, leave it alone; if a
			// paused/failed job is added again, put it back in the queue.
			// That is the user's intent.
			if existing.State.Resumable() {
				existing.State = StateQueued
				existing.Error = ""
			}
			e.items[j.ID] = it
			snap := *existing
			e.mu.Unlock()
			e.changed(snap)
			return nil
		}
		e.jobs = append(e.jobs, j)
		e.byID[j.ID] = j
		e.items[j.ID] = it
		snap := *j
		e.mu.Unlock()
		added++
		e.changed(snap)
		return nil
	})
	for _, ie := range itemErrs {
		e.opt.Events.Errorf("item: %v", ie)
	}
	e.scheduleSave()
	e.kick()
	if err != nil {
		return added, err
	}
	return added, nil
}

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

// RetryWaiting puts every job waiting for quota back in the queue right away
// ("I switched the VPN, try now"). It doesn't touch the user's global pause.
func (e *Engine) RetryWaiting() {
	e.mu.Lock()
	var snaps []Job
	for _, j := range e.jobs {
		if j.State == StateWaiting {
			j.State = StateQueued
			j.RetryAt = time.Time{}
			j.Error = ""
			delete(e.holdSince, j.Site)
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

// ---------- Scheduler ----------

// Run drives the scheduler until ctx ends. On exit it cancels the running
// jobs; they are written to the queue file as "queued" and continue by
// themselves on the next launch.
func (e *Engine) Run(ctx context.Context) {
	e.mu.Lock()
	e.runCtx = ctx
	e.mu.Unlock()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		now := time.Now()
		e.releaseDue(now)
		pn, pok := e.startProbes(ctx, now)
		e.dispatch(ctx)
		// If there are waiting jobs: wake at the nearest RetryAt or the next probe.
		next, ok := e.nextRetry()
		if pok && (!ok || pn.Before(next)) {
			next, ok = pn, true
		}
		if ok {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(time.Until(next) + 50*time.Millisecond)
		}
		select {
		case <-ctx.Done():
			e.shutdown()
			return
		case <-e.wake:
		case <-timer.C:
		}
	}
}

// DefaultQuotaWait is the retry interval when the site doesn't tell its reset time.
const DefaultQuotaWait = 10 * time.Minute

// nextRetry returns the earliest RetryAt of the waiting jobs and the earliest
// opening of the sites held because of a network outage: the timer must wake
// at both.
func (e *Engine) nextRetry() (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var next time.Time
	for _, j := range e.jobs {
		if j.State == StateWaiting && (next.IsZero() || j.RetryAt.Before(next)) {
			next = j.RetryAt
		}
	}
	now := time.Now()
	for _, until := range e.netBackoffUntil {
		if until.After(now) && (next.IsZero() || until.Before(next)) {
			next = until
		}
	}
	return next, !next.IsZero()
}

// releaseDue puts waiting jobs whose time has come back in the queue.
func (e *Engine) releaseDue(now time.Time) {
	e.mu.Lock()
	var snaps []Job
	for _, j := range e.jobs {
		if j.State == StateWaiting && !j.RetryAt.After(now) {
			j.State = StateQueued
			j.RetryAt = time.Time{}
			j.Error = ""
			delete(e.holdSince, j.Site)
			snaps = append(snaps, *j)
		}
	}
	e.mu.Unlock()
	for _, s := range snaps {
		e.changed(s)
	}
	if len(snaps) > 0 {
		e.scheduleSave()
	}
}

// startProbes takes the lock itself: for every site with jobs waiting for
// quota whose turn has come, it starts a probe in the background
// (probeQuota) about the site's first waiting job. It returns the earliest
// next probe time (for the timer).
func (e *Engine) startProbes(ctx context.Context, now time.Time) (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	first := map[string]*Job{}
	for _, j := range e.jobs {
		if j.State == StateWaiting && first[j.Site] == nil {
			first[j.Site] = j
		}
	}
	var next time.Time
	for name, j := range first {
		if e.probing[name] {
			continue
		}
		at, scheduled := e.nextProbe[name]
		if !scheduled {
			at = now.Add(e.opt.QuotaProbeEvery)
			e.nextProbe[name] = at
		}
		if at.After(now) {
			if next.IsZero() || at.Before(next) {
				next = at
			}
			continue
		}
		w, ok := e.workers[name]
		if !ok {
			continue
		}
		e.probing[name] = true
		e.nextProbe[name] = now.Add(e.opt.QuotaProbeEvery)
		go e.probeQuota(ctx, w, name, j.ID, j.SourcePage)
		if next.IsZero() || e.nextProbe[name].Before(next) {
			next = e.nextProbe[name]
		}
	}
	return next, !next.IsZero()
}

// probeQuota asks whether siteName's quota has opened, with the smallest
// request that is still a real transfer: the first byte of one of its
// waiting files. It runs in its own goroutine, at most one per site.
//
//   - A byte came back: the quota is open (the time passed or the VPN was
//     switched). EVERY waiting job of the site goes back into the queue at
//     once; nobody waits for a whole file to download first.
//   - The quota answer again: the next probe in QuotaProbeEvery.
//   - The network is down: likewise.
//   - The file itself can't be had (deleted, forbidden): that job fails, and
//     the next probe asks about another file right away. Otherwise a dead
//     file at the head of the queue would keep the site waiting for good.
func (e *Engine) probeQuota(ctx context.Context, w *run.Worker, siteName, jobID, sourcePage string) {
	err := e.checkSite(ctx, w, siteName, sourcePage)

	e.mu.Lock()
	delete(e.probing, siteName)
	var snaps []Job
	opened := false
	switch {
	case err == nil:
		snaps = e.releaseWaiting(siteName)
		opened = len(snaps) > 0
	case ctx.Err() != nil:
		// Shutting down.
	default:
		e.nextProbe[siteName] = time.Now().Add(e.opt.QuotaProbeEvery)
		if permanentProbeError(err) {
			delete(e.probeItem, siteName)
			if j, ok := e.byID[jobID]; ok && j.State == StateWaiting {
				j.State = StateFailed
				j.RetryAt = time.Time{}
				j.Error = err.Error()
				snaps = append(snaps, *j)
				e.nextProbe[siteName] = time.Now()
			}
		}
	}
	e.mu.Unlock()

	for _, s := range snaps {
		e.changed(s)
	}
	if opened {
		e.noticeOpened(siteName, len(snaps))
	}
	if len(snaps) > 0 {
		e.scheduleSave()
		e.kick()
	}
}

// checkSite asks the site for the first byte of sourcePage's file. The
// resolved item is kept between probes, so a probe is one small request, not
// an API call as well. If its URL has expired (mega binds it to the IP that
// asked for it: exactly what a VPN switch does) it is resolved again and
// asked once more straight away.
func (e *Engine) checkSite(ctx context.Context, w *run.Worker, siteName, sourcePage string) error {
	var it site.Item
	e.mu.Lock()
	if c, ok := e.probeItem[siteName]; ok && c.SourcePage == sourcePage {
		it = c
	}
	e.mu.Unlock()
	for attempt := 0; ; attempt++ {
		if it.URL == "" {
			fresh, err := w.Resolver.ResolveOne(ctx, sourcePage)
			if err != nil {
				return err
			}
			it = fresh
			e.mu.Lock()
			e.probeItem[siteName] = it
			e.mu.Unlock()
		}
		err := w.Down.CheckAccess(ctx, it)
		if dl.IsURLExpired(err) && attempt == 0 {
			it = site.Item{}
			continue
		}
		return err
	}
}

// permanentProbeError: the probed file itself can't be had (deleted,
// forbidden), as opposed to the quota still being exhausted or the network
// being down.
func permanentProbeError(err error) bool {
	if _, ok := site.QuotaOf(err); ok {
		return false
	}
	if snet.IsTransient(err) || errors.Is(err, site.ErrAllDomainsBurned) {
		return false
	}
	var rt interface{ Retryable() bool }
	return !errors.As(err, &rt) || !rt.Retryable()
}

// noticeOpened tells the user a site's quota opened, at most once a minute
// per site: if the quota closes again right away, the probe and the release
// repeat every few seconds and the status line mustn't flicker with it.
func (e *Engine) noticeOpened(siteName string, n int) {
	e.mu.Lock()
	last := e.openNotified[siteName]
	quiet := time.Since(last) < time.Minute
	if !quiet {
		e.openNotified[siteName] = time.Now()
	}
	e.mu.Unlock()
	if !quiet {
		e.notice(fmt.Sprintf("%s: allowance available again, %d waiting jobs are back in the queue.", siteName, n))
	}
}

// enterQuotaWait, under the lock: when the quota runs out it puts the job and
// the same site's queued jobs on hold. The returned snapshots are published
// outside the lock.
//
// The same site's queued jobs wait too: the quota is per IP and site wide;
// each would start in turn and get the same 509. Running ones are NOT
// TOUCHED: mega doesn't cut a transfer that already started, letting it
// finish is a gain.
//
// The resolved Item is dropped: mega's download URL is bound to the IP that
// requested it. When the user switches VPN a fresh "g" is needed; the old URL
// would only cost a round of 403.
func (e *Engine) enterQuotaWait(j *Job, wait time.Duration, msg string) []Job {
	if wait <= 0 {
		wait = DefaultQuotaWait
	}
	now := time.Now()
	until := now.Add(wait)
	e.holdSince[j.Site] = now
	if !e.probing[j.Site] {
		e.nextProbe[j.Site] = now.Add(e.opt.QuotaProbeEvery)
	}
	var snaps []Job
	for _, o := range e.jobs {
		if o.Site != j.Site {
			continue
		}
		if o != j && o.State != StateQueued {
			continue
		}
		o.State = StateWaiting
		o.RetryAt = until
		o.Error = msg
		delete(e.items, o.ID)
		snaps = append(snaps, *o)
	}
	return snaps
}

// releaseSite, under the lock: if a job that started AFTER the quota ran out
// downloaded successfully (the time passed or the user changed IP and
// pressed ▶), there's no reason left for the others to wait.
func (e *Engine) releaseSite(siteName string, started time.Time) []Job {
	if since, held := e.holdSince[siteName]; !held || started.Before(since) {
		return nil
	}
	return e.releaseWaiting(siteName)
}

// releaseWaiting, under the lock: every waiting job of the site back into the queue.
func (e *Engine) releaseWaiting(siteName string) []Job {
	delete(e.holdSince, siteName)
	delete(e.nextProbe, siteName)
	delete(e.probeItem, siteName)
	var snaps []Job
	for _, o := range e.jobs {
		if o.Site == siteName && o.State == StateWaiting {
			o.State = StateQueued
			o.RetryAt = time.Time{}
			o.Error = ""
			snaps = append(snaps, *o)
		}
	}
	return snaps
}

func (e *Engine) kick() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// dispatch starts queued jobs while there are free slots.
func (e *Engine) dispatch(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pausedAll || e.closing {
		return
	}
	now := time.Now()
	// A site's share is capped by its max_concurrent. Starting more would
	// only park the job inside the worker, waiting for a file slot while its
	// row says "downloading" at 0%; left in the queue it lets another site's
	// job use the slot instead.
	perSite := map[string]int{}
	for _, j := range e.jobs {
		if j.State == StateRunning {
			perSite[j.Site]++
		}
	}
	for _, j := range e.jobs {
		if len(e.active) >= e.opt.MaxActive {
			return
		}
		if j.State != StateQueued {
			continue
		}
		if e.captchaHold[j.Site] || e.netHeld(j.Site, now) {
			continue
		}
		w, ok := e.workers[j.Site]
		if !ok {
			j.State = StateFailed
			j.Error = "no site definition: " + j.Site
			continue
		}
		if c := w.Cfg.MaxConcurrent; c > 0 && perSite[j.Site] >= c {
			continue
		}
		perSite[j.Site]++
		j.State = StateRunning
		j.Error = ""
		jctx, cancel := context.WithCancel(ctx)
		e.active[j.ID] = cancel
		item, haveItem := e.items[j.ID]
		snap := *j
		go e.runJob(jctx, w, snap, item, haveItem)
	}
}

// runJob drives a single job from start to finish and records the result in the queue.
func (e *Engine) runJob(ctx context.Context, w *run.Worker, j Job, it site.Item, haveItem bool) {
	started := time.Now()
	e.changed(j)
	e.scheduleSave()

	outcome := e.execute(ctx, w, &j, it, haveItem)

	e.mu.Lock()
	live, still := e.byID[j.ID]
	delete(e.active, j.ID)
	wantedPause := e.pauseWant[j.ID]
	delete(e.pauseWant, j.ID)
	wipe := e.wipeWant[j.ID]
	delete(e.wipeWant, j.ID)
	closing := e.closing
	var extra []Job // the same site's other affected jobs
	quotaHit := false
	var netWait time.Duration
	netNotice := false
	if still {
		live.Path, live.Filename = j.Path, j.Filename
		live.Conns = 0
		if j.Size > 0 {
			live.Size = j.Size
		}
		switch outcome.Kind {
		case run.OutcomeDone:
			live.State = StateDone
			live.Done = outcome.Result.Size
			live.Path = outcome.Result.Path
			live.Filename = filepath.Base(outcome.Result.Path)
			live.FinishedAt = time.Now()
			live.Error = ""
			extra = e.releaseSite(live.Site, started)
		case run.OutcomeSkipped:
			live.State = StateSkipped
			live.Done = outcome.Entry.Size
			live.Size = outcome.Entry.Size
			live.Path = filepath.Join(j.OutDir, outcome.Entry.Path)
			live.FinishedAt = time.Now()
		case run.OutcomeStopped:
			live.State = StateStopped
			live.Error = outcome.Err.Error()
		case run.OutcomeCanceled:
			switch {
			case closing:
				// The app is closing: let it continue by itself on the next launch.
				live.State = StateQueued
			case wantedPause:
				live.State = StatePaused
			default:
				live.State = StateQueued
			}
		default:
			if q, ok := site.QuotaOf(outcome.Err); ok {
				extra = e.enterQuotaWait(live, q.Wait, outcome.Err.Error())
				quotaHit = true
				break
			}
			// The network was unreachable: the job doesn't fail, it stays in
			// the queue; the site is held for a while. If the budget ran out,
			// "failed" as before.
			var nd *netDownError
			if errors.As(outcome.Err, &nd) {
				if wait, keep, fresh := e.netHold(live.Site, time.Now()); keep {
					live.State = StateQueued
					live.Error = ""
					netWait, netNotice = wait, fresh
					break
				}
			}
			live.State = StateFailed
			if outcome.Err != nil {
				live.Error = outcome.Err.Error()
			}
		}
		if outcome.Kind == run.OutcomeStopped {
			// A captcha affects THAT SITE (pixeldrain's per-file counter):
			// don't start the site's other jobs for nothing, let other sites go on.
			e.captchaHold[live.Site] = true
		}
	}
	var snap Job
	if still {
		snap = *live
	}
	e.mu.Unlock()

	if !still && wipe && j.Path != "" {
		wipePartial(j.Path)
	}
	if still {
		e.changed(snap)
	}
	for _, x := range extra {
		if x.ID != snap.ID {
			e.changed(x)
		}
	}
	if outcome.Kind == run.OutcomeDone && len(extra) > 0 {
		// A job that started after the quota ran out downloaded: the quota opened
		// (the time passed or the IP changed; the user pressed ▶ on one job).
		e.noticeOpened(snap.Site, len(extra))
	}
	if quotaHit {
		e.notifyQuotaHold(snap.Site, snap.RetryAt)
		e.maybeRunQuotaCommand()
	}
	if netNotice {
		e.notice(fmt.Sprintf("%s: network unreachable (%s); retrying in %s.",
			snap.Site, firstLineOf(outcome.Err.Error()), humanWait(netWait)))
	}
	e.scheduleSave()
	e.kick()
}

// notifyQuotaHold calls OnQuotaHold sparingly, per site.
func (e *Engine) notifyQuotaHold(siteName string, retryAt time.Time) {
	if e.opt.OnQuotaHold == nil {
		return
	}
	e.mu.Lock()
	last := e.quotaNotified[siteName]
	if time.Since(last) < quotaHoldNotifyEvery {
		e.mu.Unlock()
		return
	}
	e.quotaNotified[siteName] = time.Now()
	e.mu.Unlock()
	go e.opt.OnQuotaHold(siteName, retryAt)
}

// ---------- Quota command ----------

// SetQuotaCommand sets the command line to run when the quota runs out; an
// empty line turns it off. It is the user's own script: typically a command
// that switches VPN server. It runs in a shell (Windows: cmd /S /C) and its
// output goes into the notification.
func (e *Engine) SetQuotaCommand(line string) {
	e.mu.Lock()
	e.quotaCmd = strings.TrimSpace(line)
	e.mu.Unlock()
}

func (e *Engine) QuotaCommand() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.quotaCmd
}

// maybeRunQuotaCommand runs the command in the background if one is set and
// the cooldown has passed. When it finishes it pulls the probe forward: if
// the IP changed it is noticed without waiting for the next probe.
func (e *Engine) maybeRunQuotaCommand() {
	e.mu.Lock()
	line := e.quotaCmd
	ctx := e.runCtx
	if line == "" || e.quotaCmdRunning || time.Since(e.quotaCmdLast) < QuotaCommandCooldown {
		e.mu.Unlock()
		return
	}
	e.quotaCmdRunning = true
	e.quotaCmdLast = time.Now()
	e.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}

	e.notice("Quota exceeded, running the command: " + line)
	go func() {
		start := time.Now()
		out, err := hook.Run(ctx, line, 0)
		took := time.Since(start).Round(time.Second)

		e.mu.Lock()
		e.quotaCmdRunning = false
		if err == nil {
			for name := range e.holdSince {
				e.nextProbe[name] = time.Now().Add(quotaCommandProbeDelay)
			}
		}
		e.mu.Unlock()

		switch {
		case err != nil && out != "":
			e.notice(fmt.Sprintf("Quota command failed (%v): %s", err, firstLineOf(out)))
		case err != nil:
			e.notice(fmt.Sprintf("Quota command failed: %v", err))
		default:
			e.notice(fmt.Sprintf("Quota command finished (%s); retrying shortly.", took))
			e.kick()
		}
	}()
}

func firstLineOf(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func (e *Engine) notice(msg string) {
	e.opt.Events.Infof("%s", msg)
	if e.opt.OnNotice != nil {
		e.opt.OnNotice(msg)
	}
}

// execute hands the job to the Worker; without an Item it re-resolves from SourcePage.
func (e *Engine) execute(ctx context.Context, w *run.Worker, j *Job, it site.Item, haveItem bool) run.Outcome {
	if !haveItem {
		fresh, err := w.Resolver.ResolveOne(ctx, j.SourcePage)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return run.Outcome{Kind: run.OutcomeCanceled, Err: err}
			}
			err = fmt.Errorf("re-resolve: %w", err)
			// If the network is unreachable (or every bunkr domain was
			// eliminated because of the network) the job doesn't fail; runJob
			// holds the site for a while.
			if snet.IsTransient(err) || errors.Is(err, site.ErrAllDomainsBurned) {
				err = &netDownError{err: err}
			}
			return run.Outcome{Kind: run.OutcomeFailed, Err: err}
		}
		// Resolution passed: the network is there, reset the backoff counter.
		e.mu.Lock()
		delete(e.netFails, j.Site)
		e.mu.Unlock()
		it = fresh
		it.Dir, it.Index = j.Dir, j.Index
		if j.Filename != "" {
			it.Filename = j.Filename
		}
	}

	// The final name is decided BEFORE the download and written to the queue:
	// that is the guarantee of continuing under the same name when the app is
	// closed and reopened.
	if path, err := w.Down.Plan(j.OutDir, it); err == nil {
		j.Path = path
		j.Filename = filepath.Base(path)
		it.Filename = j.Filename
	}
	if it.Size > 0 {
		j.Size = it.Size
	}

	ledger, err := e.ledgerFor(j.OutDir)
	if err != nil {
		return run.Outcome{Kind: run.OutcomeFailed, Err: err}
	}
	return w.DownloadItem(ctx, j.OutDir, ledger, it, e.opt.Events)
}

// netDownError marks that resolution failed because the network was
// unreachable (see snet.IsTransient).
type netDownError struct{ err error }

func (e *netDownError) Error() string { return e.err.Error() }
func (e *netDownError) Unwrap() error { return e.err }

// netHold, under the lock: holds the jobs of a site that got a network error.
// The first return value is the wait, the second whether the job stays in the
// queue (false: the budget ran out, let it fail), the third whether a new
// hold window was opened (notify only then).
//
// Other jobs failing within the same window DON'T increase the counter: when
// three jobs start at once and hit the same outage, the budget must not run
// out three times as fast.
func (e *Engine) netHold(siteName string, now time.Time) (time.Duration, bool, bool) {
	if until, ok := e.netBackoffUntil[siteName]; ok && now.Before(until) {
		return until.Sub(now), true, false
	}
	n := e.netFails[siteName] + 1
	e.netFails[siteName] = n
	if n > maxNetFails {
		return 0, false, false
	}
	wait := e.opt.NetRetryBase << uint(n-1)
	if wait > maxNetRetryWait || wait <= 0 {
		wait = maxNetRetryWait
	}
	e.netBackoffUntil[siteName] = now.Add(wait)
	return wait, true, true
}

// netHeld, under the lock: are the site's jobs waiting because of a network outage?
func (e *Engine) netHeld(siteName string, now time.Time) bool {
	until, ok := e.netBackoffUntil[siteName]
	return ok && now.Before(until)
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

// humanWait writes short durations in seconds and longer ones with FormatWait.
func humanWait(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	}
	return site.FormatWait(d)
}

// ledgerFor opens the output folder's ledger or returns it from the cache.
func (e *Engine) ledgerFor(outDir string) (*store.Ledger, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if l, ok := e.ledgers[outDir]; ok {
		return l, nil
	}
	l, err := store.Open(outDir)
	if err != nil {
		return nil, err
	}
	e.ledgers[outDir] = l
	return l, nil
}

// onConnections writes the number of connections in use into the matching job.
func (e *Engine) onConnections(it site.Item, n int) {
	e.mu.Lock()
	var snap *Job
	for _, j := range e.jobs {
		if j.State == StateRunning && j.SourcePage == it.SourcePage && j.Dir == it.Dir {
			if j.Conns != n {
				j.Conns = n
				s := *j
				snap = &s
			}
			break
		}
	}
	e.mu.Unlock()
	if snap != nil {
		e.changed(*snap)
	}
}

// onProgress writes the downloader's progress into the matching job.
func (e *Engine) onProgress(it site.Item, done, total int64) {
	e.mu.Lock()
	var snap *Job
	for _, j := range e.jobs {
		if j.State == StateRunning && j.SourcePage == it.SourcePage && j.Dir == it.Dir {
			j.Done = done
			if total > 0 {
				j.Size = total
			}
			s := *j
			snap = &s
			break
		}
	}
	e.mu.Unlock()
	if snap != nil {
		e.changed(*snap)
	}
}

func (e *Engine) changed(j Job) {
	if e.opt.OnChange != nil {
		e.opt.OnChange(j)
	}
}

// ---------- Persistence and shutdown ----------

func (e *Engine) scheduleSave() {
	if e.opt.StatePath == "" {
		return
	}
	e.saveMu.Lock()
	defer e.saveMu.Unlock()
	if e.saveTimer != nil {
		return
	}
	e.saveTimer = time.AfterFunc(saveDebounce, func() {
		e.saveMu.Lock()
		e.saveTimer = nil
		e.saveMu.Unlock()
		e.flush()
	})
}

// flush writes the queue right away.
func (e *Engine) flush() {
	if e.opt.StatePath == "" {
		return
	}
	e.mu.Lock()
	jobs := make([]*Job, len(e.jobs))
	for i, j := range e.jobs {
		c := *j
		jobs[i] = &c
	}
	e.mu.Unlock()
	sort.SliceStable(jobs, func(a, b int) bool { return jobs[a].AddedAt.Before(jobs[b].AddedAt) })
	if err := save(e.opt.StatePath, jobs); err != nil {
		e.opt.Events.Errorf("could not write the queue: %v", err)
	}
}

// shutdown cancels the running jobs and waits for them to finish.
func (e *Engine) shutdown() {
	e.mu.Lock()
	e.closing = true
	cancels := make([]context.CancelFunc, 0, len(e.active))
	for _, c := range e.active {
		cancels = append(cancels, c)
	}
	e.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	// When jobs end the runJobs drop out of active; wait a short while so
	// the final state is written to disk.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		n := len(e.active)
		e.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.saveMu.Lock()
	if e.saveTimer != nil {
		e.saveTimer.Stop()
		e.saveTimer = nil
	}
	e.saveMu.Unlock()
	e.flush()

	e.mu.Lock()
	for _, l := range e.ledgers {
		_ = l.Close()
	}
	e.ledgers = map[string]*store.Ledger{}
	e.mu.Unlock()
}
