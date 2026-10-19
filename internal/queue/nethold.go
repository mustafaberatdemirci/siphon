package queue

// Network outages: a site whose links can't be resolved because the network
// is unreachable is held for a while instead of failing its jobs.

import (
	"fmt"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

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

// humanWait writes short durations in seconds and longer ones with FormatWait.
func humanWait(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	}
	return site.FormatWait(d)
}
