package queue

// Blocked sites: a link that can't be added, or a download that keeps
// timing out, may be the user's network blocking the site (measured: a
// provider answering bunkr.cr and cyberfile.me with its block page and
// cutting handshakes naming www.mediafire.com). The engine checks once in a
// while (internal/netcheck) and says so; "timeout" alone sent the user
// looking for the fault in Siphon.

import (
	"context"
	"fmt"
	"time"

	snet "github.com/mustafaberatdemirci/siphon/internal/net"
	"github.com/mustafaberatdemirci/siphon/internal/netcheck"
)

// netCheckEvery: a host's network check is made again after this long at
// the earliest (the user may have turned a VPN on).
const netCheckEvery = 10 * time.Minute

type netCheck struct {
	at      time.Time
	summary string
	blocked bool
	running bool
}

// explainNetwork: if err is a network error and the user's network blocks
// the host it happened on (the URL in err, else fallbackURL's host), the
// finding's one line. fresh is set when the check ran now, not from before:
// a notice is worth giving only then.
func (e *Engine) explainNetwork(ctx context.Context, err error, fallbackURL string) (summary string, fresh bool) {
	if e.opt.CheckNetwork == nil || !snet.IsTransient(err) {
		return "", false
	}
	host := netcheck.HostOfError(err, fallbackURL)
	if host == "" {
		return "", false
	}
	e.mu.Lock()
	c := e.netChecks[host]
	if c.running || !c.at.IsZero() && time.Since(c.at) < netCheckEvery {
		e.mu.Unlock()
		if c.blocked {
			return c.summary, false
		}
		return "", false
	}
	e.netChecks[host] = netCheck{at: c.at, summary: c.summary, blocked: c.blocked, running: true}
	e.mu.Unlock()

	f := e.opt.CheckNetwork(ctx, host)

	e.mu.Lock()
	e.netChecks[host] = netCheck{at: time.Now(), summary: f.Summary(), blocked: f.Blocked()}
	e.mu.Unlock()
	if !f.Blocked() {
		return "", false
	}
	e.opt.Events.Infof("network check: %s (%s)", f.Summary(), f.Evidence())
	return f.Summary(), true
}

// explainLater checks the network in the background after a job's network
// error and tells the user when the site is blocked.
func (e *Engine) explainLater(err error, fallbackURL string) {
	if e.opt.CheckNetwork == nil || !snet.IsTransient(err) {
		return
	}
	e.mu.Lock()
	ctx := e.runCtx
	e.mu.Unlock()
	if ctx == nil {
		return
	}
	go func() {
		if msg, fresh := e.explainNetwork(ctx, err, fallbackURL); fresh {
			e.notice(msg)
		}
	}()
}

// withNetworkNote puts the network's finding in front of an Add error.
func (e *Engine) withNetworkNote(ctx context.Context, err error, rawURL string) error {
	if msg, _ := e.explainNetwork(ctx, err, rawURL); msg != "" {
		return fmt.Errorf("%s (%w)", msg, err)
	}
	return err
}
