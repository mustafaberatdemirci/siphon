package queue

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	snet "github.com/mustafaberatdemirci/siphon/internal/net"
)

// outageTransport fails the next n requests the way a network that is down
// does, then lets them through.
type outageTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	n    int
}

func (o *outageTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	o.mu.Lock()
	down := o.n > 0
	if down {
		o.n--
	}
	o.mu.Unlock()
	if down {
		return nil, netDown
	}
	return o.base.RoundTrip(r)
}

// cutNetwork makes the fake site's downloads fail n times with a network
// error, and its retries quick.
func cutNetwork(h *harness, n int) {
	w := h.e.workers["fake"]
	w.Down.Client = &http.Client{Transport: &outageTransport{base: h.f.srv.Client().Transport, n: n}}
	w.Policy.Backoff = snet.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond}
}

// WHAT THE USER WOULD EXPERIENCE: the router restarts while a queue added in
// this session downloads. The links are resolved already, so nothing went
// through the "network is down" hold of re-resolution: each download spent
// its retries in seconds and fell into "failed", then the next, and after a
// few minutes the whole queue was red. A network error while downloading
// must keep the job queued too.
func TestNetworkOutageWhileDownloadingKeepsJobsQueued(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
	h := newHarness(t, f, "", 1)
	h.e.opt.NetRetryBase = 20 * time.Millisecond
	var mu sync.Mutex
	var notices []string
	h.e.opt.OnNotice = func(s string) { mu.Lock(); notices = append(notices, s); mu.Unlock() }
	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	// More failures than one job's retries (2): the outage outlasts them.
	cutNetwork(h, 5)
	h.start()
	defer h.stop()

	h.waitAll(StateDone, 15*time.Second)
	for _, j := range h.e.Jobs() {
		if h.sawState(j.ID, StateFailed) {
			t.Errorf("%s fell into 'failed' on a network error while downloading", j.Filename)
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
		t.Errorf("the outage was not reported to the user: %v", notices)
	}
}

// A network that never comes back still ends in "failed", and a file that
// came in between gives the next outage its whole budget back.
func TestNetworkOutageWhileDownloadingGivesUpEventually(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	h := newHarness(t, f, "", 1)
	h.e.opt.NetRetryBase = time.Millisecond
	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	cutNetwork(h, 1<<30)
	h.start()
	defer h.stop()
	a := h.waitState(jobByName(h.e, "a.bin").ID, StateFailed, 15*time.Second)

	// The network is back: Retry failed downloads it, and the budget is new.
	cutNetwork(h, 0)
	h.e.RetryFailed()
	h.waitState(a.ID, StateDone, 10*time.Second)
	h.e.mu.Lock()
	fails, held := h.e.netFails["fake"], h.e.netHeld("fake", time.Now())
	h.e.mu.Unlock()
	if fails != 0 || held {
		t.Errorf("after a finished download: %d outages counted, held=%v", fails, held)
	}
}
