package queue

import (
	"context"
	"testing"
	"time"
)

// Outside the download hours a running download stops and stays queued, not
// paused; nothing starts; when the hours come it goes on from where it was.
func TestOffHoursHoldsAndReleases(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin")
	h := newHarness(t, f, "", 1)
	hold := f.holdNext("a.bin")
	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Add(context.Background(), "file://b.bin", h.out); err != nil {
		t.Fatal(err)
	}
	h.start()
	defer h.stop()
	a := jobByName(h.e, "a.bin").ID
	h.waitState(a, StateRunning, 5*time.Second)

	h.e.SetOffHours(true)
	close(hold)
	h.waitState(a, StateQueued, 5*time.Second)
	time.Sleep(200 * time.Millisecond) // a dispatch would have started one by now
	for _, j := range h.e.Jobs() {
		if j.State != StateQueued {
			t.Fatalf("%s is %s outside the download hours", j.Filename, j.State)
		}
	}
	if h.sawState(a, StatePaused) {
		t.Error("the stopped download was shown as paused")
	}
	if h.e.Paused() {
		t.Error("the hours turned the user's Pause all on")
	}

	h.e.SetOffHours(false)
	h.waitAll(StateDone, 10*time.Second)
	if r := f.rangesOf("a.bin"); len(r) < 2 || r[len(r)-1] == "" {
		t.Errorf("a.bin started over instead of going on: ranges %q", r)
	}
}
