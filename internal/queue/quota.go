package queue

// The site quota: when a site's per-IP transfer quota runs out (mega) its
// jobs wait instead of failing, the queue keeps checking whether the quota
// opened (a VPN switch, the reset time), releases them all at once, and can
// run the user's own VPN switch command.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/dl"
	"github.com/mustafaberatdemirci/siphon/internal/hook"
	snet "github.com/mustafaberatdemirci/siphon/internal/net"
	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

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

// quotaHoldNotifyEvery: OnQuotaHold at most this often for the same site. If
// the probe says "there is allowance" and the CDN gives 509 again the hold is
// set up again; notifications must not rain down each time.
const quotaHoldNotifyEvery = 10 * time.Minute

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

// DefaultQuotaWait is the retry interval when the site doesn't tell its reset time.
const DefaultQuotaWait = 10 * time.Minute

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
