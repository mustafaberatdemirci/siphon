package queue

// Busy sites: when a site's server answers "too many connections" (503 or
// 429) after a download's retries, the job doesn't fail. If others of the
// site's jobs were running, the server allows fewer at once: the queue runs
// that many from then on (and tries one more every capRaiseAfter, since
// sites change their rules both ways). If it was alone, the server is busy:
// the site is held a while, like a network outage, and the job fails only
// once that budget is spent.
//
// Why: bunkr and cyberfile cut their allowance to one connection per IP
// address while Siphon ran four downloads of theirs; every download but one
// spent its retries on 503/429 and a whole album failed.

import (
	"fmt"
	"time"
)

const (
	// DefaultBusyRetryBase is the first wait of a busy site (doubled on each
	// consecutive refusal, up to maxBusyRetryWait).
	DefaultBusyRetryBase = 30 * time.Second
	maxBusyRetryWait     = 5 * time.Minute
	maxBusyFails         = 6

	// capRaiseAfter: a lowered limit of a site goes up by one this long after
	// it last changed, until it is the configured one again.
	capRaiseAfter = 15 * time.Minute
)

// overloaded, under the lock: live, a job of its site, ended with "too many
// connections". It reports whether the job goes back into the queue, and a
// notice for the user ("" when there is nothing new to say).
func (e *Engine) overloaded(live *Job, now time.Time) (keep bool, notice string) {
	site := live.Site
	others := 0
	for _, o := range e.jobs {
		if o != live && o.Site == site && o.State == StateRunning {
			others++
		}
	}
	if others > 0 {
		if c, ok := e.siteCap[site]; !ok || others < c {
			e.siteCap[site] = others
			e.siteCapAt[site] = now
			return true, fmt.Sprintf("%s: the server allows fewer connections; downloading %d at a time.", site, others)
		}
		return true, ""
	}
	// Alone and still refused: the server is busy.
	if until, held := e.busyUntil[site]; held && now.Before(until) {
		return true, ""
	}
	n := e.busyFails[site] + 1
	e.busyFails[site] = n
	if n > maxBusyFails {
		return false, ""
	}
	wait := e.opt.BusyRetryBase << uint(n-1)
	if wait > maxBusyRetryWait || wait <= 0 {
		wait = maxBusyRetryWait
	}
	e.busyUntil[site] = now.Add(wait)
	return true, fmt.Sprintf("%s: the server is busy (too many connections); retrying in %s.", site, humanWait(wait))
}

// siteLimit, under the lock: how many of the site's jobs may run at once
// now. A lowered limit rises by one every capRaiseAfter, up to configured.
func (e *Engine) siteLimit(site string, configured int, now time.Time) int {
	c, lowered := e.siteCap[site]
	if !lowered {
		return configured
	}
	top := configured
	if top <= 0 { // no limit of its own: the queue's is the most that runs
		top = e.opt.MaxActive
	}
	if now.Sub(e.siteCapAt[site]) >= capRaiseAfter {
		c++
		if c >= top {
			delete(e.siteCap, site)
			delete(e.siteCapAt, site)
			return configured
		}
		e.siteCap[site], e.siteCapAt[site] = c, now
	}
	return c
}

// busyHeld, under the lock: are the site's jobs waiting for its server?
func (e *Engine) busyHeld(site string, now time.Time) bool {
	until, ok := e.busyUntil[site]
	return ok && now.Before(until)
}

// clearBusy, under the lock: the site answered, or the user says go.
func (e *Engine) clearBusy(site string) {
	delete(e.busyFails, site)
	delete(e.busyUntil, site)
}
