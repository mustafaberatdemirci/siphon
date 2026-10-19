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
	snet "github.com/mustafaberatdemirci/siphon/internal/net"
	"github.com/mustafaberatdemirci/siphon/internal/netcheck"
	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/store"
)

// DefaultSegments is the requested connections per file if the user didn't
// set it. The site ceiling (max_segments) clips it: 1 on pixeldrain, bunkr
// and cyberfile, 8 on mega.
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

	// BusyRetryBase is the first wait for a site whose server refuses even a
	// single download with "too many connections" (doubled on each
	// consecutive refusal); 0 means DefaultBusyRetryBase.
	BusyRetryBase time.Duration

	// CheckNetwork, if set, is asked whether the user's network blocks a
	// host after a network error (netcheck.Check; netblock.go). Optional.
	CheckNetwork func(ctx context.Context, host string) netcheck.Finding

	// For tests: if given, the config isn't loaded and these are used.
	Resolvers []site.Resolver
	Configs   []site.SiteConfig
}

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
	// Busy servers (busy.go): site -> jobs aren't started until this moment /
	// consecutive refusals; and the lowered number of the site's jobs that
	// may run at once / when it last changed.
	busyUntil map[string]time.Time
	busyFails map[string]int
	siteCap   map[string]int
	siteCapAt map[string]time.Time
	// netChecks: host -> the last network check (netblock.go).
	netChecks map[string]netCheck

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

		busyUntil: map[string]time.Time{},
		busyFails: map[string]int{},
		siteCap:   map[string]int{},
		siteCapAt: map[string]time.Time{},
		netChecks: map[string]netCheck{},
	}
	if e.opt.QuotaProbeEvery <= 0 {
		e.opt.QuotaProbeEvery = DefaultQuotaProbeEvery
	}
	if e.opt.NetRetryBase <= 0 {
		e.opt.NetRetryBase = DefaultNetRetryBase
	}
	if e.opt.BusyRetryBase <= 0 {
		e.opt.BusyRetryBase = DefaultBusyRetryBase
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
			Name:       it.Filename,
			Size:       it.Size,
			Index:      it.Index,
			State:      StateQueued,
			AddedAt:    time.Now(),
		}
		e.mu.Lock()
		var snap Job
		if existing, dup := e.byID[j.ID]; dup {
			// If a finished job is added again, leave it alone; if a
			// paused/failed job is added again, put it back in the queue.
			// That is the user's intent.
			if existing.State.Resumable() {
				existing.State = StateQueued
				existing.Error = ""
			}
			e.items[j.ID] = it
			snap = *existing
		} else {
			e.jobs = append(e.jobs, j)
			e.byID[j.ID] = j
			e.items[j.ID] = it
			snap = *j
			added++
		}
		e.mu.Unlock()
		e.changed(snap)
		// Each job may start as soon as it is queued, not when the whole
		// link is resolved: a bunkr album asks the API once per file, and
		// a large one kept its first download waiting for minutes.
		e.scheduleSave()
		e.kick()
		return nil
	})
	for _, ie := range itemErrs {
		e.opt.Events.Errorf("item: %v", ie)
	}
	e.scheduleSave()
	e.kick()
	if err != nil {
		return added, e.withNetworkNote(ctx, err, rawURL)
	}
	return added, nil
}

// Recognizes reports whether one of the real sites (not the plain file link
// fallback) takes the link: the clipboard watch offers only those.
func (e *Engine) Recognizes(u string) bool { return run.Recognized(e.resolvers, u) }

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
	for _, holds := range []map[string]time.Time{e.netBackoffUntil, e.busyUntil} {
		for _, until := range holds {
			if until.After(now) && (next.IsZero() || until.Before(next)) {
				next = until
			}
		}
	}
	return next, !next.IsZero()
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
	if e.pausedAll || e.closing {
		e.mu.Unlock()
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
	var failed []Job // jobs of a site no longer defined; told outside the lock
	for _, j := range e.jobs {
		if len(e.active) >= e.opt.MaxActive {
			break
		}
		if j.State != StateQueued {
			continue
		}
		// The job's previous run (it was removed and added again) hasn't
		// stopped yet. Starting now would put two downloads on the same
		// .part; that run kicks the scheduler when it ends.
		if _, busy := e.active[j.ID]; busy {
			continue
		}
		if e.captchaHold[j.Site] || e.netHeld(j.Site, now) || e.busyHeld(j.Site, now) {
			continue
		}
		w, ok := e.workers[j.Site]
		if !ok {
			j.State = StateFailed
			j.Error = "no site definition: " + j.Site
			failed = append(failed, *j)
			continue
		}
		if c := e.siteLimit(j.Site, w.Cfg.MaxConcurrent, now); c > 0 && perSite[j.Site] >= c {
			continue
		}
		perSite[j.Site]++
		j.State = StateRunning
		j.Error = ""
		jctx, cancel := context.WithCancel(ctx)
		e.active[j.ID] = cancel
		item, haveItem := e.items[j.ID]
		snap := *j
		go e.runJob(jctx, w, j, snap, item, haveItem)
	}
	e.mu.Unlock()
	for _, s := range failed {
		e.changed(s)
	}
	if len(failed) > 0 {
		e.scheduleSave()
	}
}

// runJob drives a single job from start to finish and records the result in
// the queue. owner is the queue's own Job the run was started for: the result
// goes to it only, never to a job added again under the same id after this
// one was removed.
func (e *Engine) runJob(ctx context.Context, w *run.Worker, owner *Job, j Job, it site.Item, haveItem bool) {
	started := time.Now()
	e.changed(j)
	e.scheduleSave()

	outcome := e.execute(ctx, w, &j, it, haveItem)

	e.mu.Lock()
	live, still := e.byID[j.ID]
	// Removed, or removed and added again: the job under this id now is
	// another one, waiting for this run to end (see dispatch).
	still = still && live == owner
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
	var busyNotice string
	var netCause error // a failure that may be the network blocking the site
	if still {
		live.Path, live.Filename, live.Name = j.Path, j.Filename, j.Name
		live.Conns = 0
		if j.Size > 0 {
			live.Size = j.Size
		}
		switch outcome.Kind {
		case run.OutcomeDone:
			live.State = StateDone
			live.Done = outcome.Result.Size
			live.Path = outcome.Result.Path
			// A gallery ends in a folder; the row keeps its own name then.
			if fi, err := os.Stat(outcome.Result.Path); err == nil && !fi.IsDir() {
				live.Filename = filepath.Base(outcome.Result.Path)
			}
			live.FinishedAt = time.Now()
			live.Error = ""
			e.clearBusy(live.Site)
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
			// The server said "too many connections" through all the
			// retries: run fewer of the site's jobs at once, or wait for it.
			if dl.IsOverloaded(outcome.Err) {
				if keep, msg := e.overloaded(live, time.Now()); keep {
					live.State = StateQueued
					live.Error = ""
					busyNotice = msg
					break
				}
			}
			live.State = StateFailed
			if outcome.Err != nil {
				live.Error = outcome.Err.Error()
			}
			netCause = outcome.Err
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
	if busyNotice != "" {
		e.notice(busyNotice)
	}
	if netNotice {
		netCause = outcome.Err
	}
	if netCause != nil {
		e.explainLater(netCause, snap.SourcePage)
	}
	e.scheduleSave()
	e.kick()
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
		if j.Name == "" {
			j.Name = fresh.Filename
		}
		if j.Filename != "" {
			it.Filename = j.Filename
		}
	}

	if j.Name == "" { // a job of an older queue file, its item still in memory
		j.Name = it.Filename
	}

	// The final name is decided BEFORE the download and written to the queue:
	// that is the guarantee of continuing under the same name when the app is
	// closed and reopened. Not for an external tool (yt-dlp, gallery-dl): it
	// picks its own file names.
	if !w.SelfDownloads(it) {
		if path, err := w.Down.Plan(j.OutDir, it); err == nil {
			j.Path = path
			j.Filename = filepath.Base(path)
			it.Filename = j.Filename
		}
	}
	if it.Size > 0 {
		j.Size = it.Size
	}
	// Progress and connection reports find the job by this.
	it.Tag = j.ID
	// The ledger knows the file by the site's name, as the command line does.
	it.LedgerName = j.Name

	ledger, err := e.ledgerFor(j.OutDir)
	if err != nil {
		return run.Outcome{Kind: run.OutcomeFailed, Err: err}
	}
	return w.DownloadItem(ctx, j.OutDir, ledger, it, e.opt.Events)
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

// runningJobFor, under the lock, is the running job a report about it is
// for: the one its tag names (execute sets it to the job id). Two jobs can
// share a source page and folder (one link saved into two folders), so
// matching on those is only the fallback for an untagged item.
func (e *Engine) runningJobFor(it site.Item) *Job {
	if it.Tag != "" {
		if j, ok := e.byID[it.Tag]; ok && j.State == StateRunning {
			return j
		}
		return nil
	}
	for _, j := range e.jobs {
		if j.State == StateRunning && j.SourcePage == it.SourcePage && j.Dir == it.Dir {
			return j
		}
	}
	return nil
}

// onConnections writes the number of connections in use into the matching job.
func (e *Engine) onConnections(it site.Item, n int) {
	e.mu.Lock()
	var snap *Job
	if j := e.runningJobFor(it); j != nil && j.Conns != n {
		j.Conns = n
		s := *j
		snap = &s
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
	if j := e.runningJobFor(it); j != nil {
		j.Done = done
		if total > 0 {
			j.Size = total
		}
		s := *j
		snap = &s
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
