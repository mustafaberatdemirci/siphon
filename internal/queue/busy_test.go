package queue

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// busyHarness: a fake site that retries fast, and the notices it gives.
func busyHarness(t *testing.T, f *fakeSite, maxActive int) (*harness, func() []string) {
	t.Helper()
	cfg := site.SiteConfig{Name: "fake", MaxConcurrent: 4, MaxRetries: 2,
		BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}.WithDefaults()
	h := newHarnessWith(t, f, "", maxActive, cfg)
	h.e.opt.BusyRetryBase = 10 * time.Millisecond
	var mu sync.Mutex
	var notices []string
	h.e.opt.OnNotice = func(s string) { mu.Lock(); notices = append(notices, s); mu.Unlock() }
	return h, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), notices...)
	}
}

func anyContains(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// WHAT THE USER SAW (bunkr, 2026-10-19): the CDN allows one connection per IP;
// with four downloads at once three spent their retries on 503 and fell into
// "failed", until a whole album had. Now the queue learns the limit: every
// file downloads, none fails, and the site runs one at a time.
func TestConnectionLimitLowersSiteShare(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin", "d.bin")
	f.connLimit = 1
	f.delay = 150 * time.Millisecond
	h, notices := busyHarness(t, f, 4)
	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	h.start()
	defer h.stop()
	h.waitAll(StateDone, 20*time.Second)

	for _, j := range h.e.Jobs() {
		if h.sawState(j.ID, StateFailed) {
			t.Errorf("%s fell into 'failed' on 503", j.Filename)
		}
	}
	h.e.mu.Lock()
	limit := h.e.siteLimit("fake", 4, time.Now())
	h.e.mu.Unlock()
	if limit != 1 {
		t.Errorf("the site's share is %d, want 1", limit)
	}
	if n := notices(); !anyContains(n, "fewer connections") {
		t.Errorf("the lowered share was not told: %v", n)
	}
}

// A server that refuses even a single download is busy: the job waits and
// downloads when the server answers again, without falling into "failed".
func TestBusyServerHoldsTheJob(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	f.busy = 5 // two runs of two tries, and the first try of the third
	h, notices := busyHarness(t, f, 4)
	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	h.start()
	defer h.stop()
	id := jobByName(h.e, "a.bin").ID
	h.waitState(id, StateDone, 10*time.Second)
	if h.sawState(id, StateFailed) {
		t.Error("the job fell into 'failed' while the server was busy")
	}
	if n := notices(); !anyContains(n, "busy") {
		t.Errorf("the busy server was not told: %v", n)
	}
	h.e.mu.Lock()
	fails := h.e.busyFails["fake"]
	h.e.mu.Unlock()
	if fails != 0 {
		t.Errorf("a download went through but the refusals are still counted (%d)", fails)
	}
}

// A server that never answers again: when the budget runs out the job falls
// into "failed" with the server's answer, and "Retry failed" starts over.
func TestBusyServerGivesUpEventually(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	f.busy = 1 << 30
	h, _ := busyHarness(t, f, 4)
	h.e.opt.BusyRetryBase = time.Millisecond
	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	h.start()
	defer h.stop()
	id := jobByName(h.e, "a.bin").ID
	j := h.waitState(id, StateFailed, 20*time.Second)
	if !strings.Contains(j.Error, "503") {
		t.Errorf("error %q doesn't say what the server answered", j.Error)
	}
	if got, want := f.hitCount("a.bin"), (maxBusyFails+1)*2; got != want {
		t.Errorf("%d requests, want %d (%d runs of 2 tries)", got, want, maxBusyFails+1)
	}

	f.mu.Lock()
	f.busy = 0
	f.mu.Unlock()
	h.e.RetryFailed()
	h.waitState(id, StateDone, 10*time.Second)
}

// A lowered share goes up again, one at a time, until it is the configured one.
func TestLoweredShareRisesAgain(t *testing.T) {
	f := newFakeSite(t)
	h := newHarness(t, f, "", 4)
	e := h.e
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.siteCap["fake"], e.siteCapAt["fake"] = 1, now
	if got := e.siteLimit("fake", 3, now.Add(capRaiseAfter-time.Second)); got != 1 {
		t.Errorf("before the wait: %d, want 1", got)
	}
	if got := e.siteLimit("fake", 3, now.Add(capRaiseAfter)); got != 2 {
		t.Errorf("after one wait: %d, want 2", got)
	}
	if got := e.siteLimit("fake", 3, now.Add(2*capRaiseAfter)); got != 3 {
		t.Errorf("after two waits: %d, want 3", got)
	}
	if _, lowered := e.siteCap["fake"]; lowered {
		t.Error("the share is back to the configured one but still recorded as lowered")
	}
	// A site with no limit of its own rises to the queue's.
	e.siteCap["fake"], e.siteCapAt["fake"] = 3, now
	if got := e.siteLimit("fake", 0, now.Add(capRaiseAfter)); got != 0 {
		t.Errorf("unlimited site: %d, want 0 (no limit)", got)
	}
}
