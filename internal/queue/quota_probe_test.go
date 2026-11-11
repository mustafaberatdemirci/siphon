package queue

import (
	"testing"
	"time"
)

// The quota command pulls the next probe forward when it finishes. A job of
// the same site whose 509 arrived after that pushed the probe back a whole
// interval: with a long interval the switched VPN wasn't noticed for it
// (TestQuotaCommandRunsOnceAndPullsProbeForward failed now and then on a
// slow machine this way).
func TestALater509DoesNotPushAPulledProbeBack(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
	h := newHarness(t, f, "", 2)
	h.e.opt.QuotaProbeEvery = time.Hour
	e := h.e
	jobs := []*Job{{ID: "a", Site: "fake", State: StateRunning}, {ID: "b", Site: "fake", State: StateRunning}}

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, j := range jobs {
		e.jobs = append(e.jobs, j)
		e.byID[j.ID] = j
	}
	e.enterQuotaWait(jobs[0], 0, "509")
	if due := e.nextProbe["fake"]; time.Until(due) < 50*time.Minute {
		t.Fatalf("the first 509 scheduled the probe in %s, want the interval", time.Until(due))
	}
	pulled := time.Now().Add(3 * time.Second) // what the finished command does
	e.nextProbe["fake"] = pulled
	e.enterQuotaWait(jobs[1], 0, "509")
	if due := e.nextProbe["fake"]; !due.Equal(pulled) {
		t.Errorf("a later 509 moved the pulled probe to %s from now", time.Until(due).Round(time.Second))
	}
}
