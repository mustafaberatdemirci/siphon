package main

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakePower replaces the sleep action and counts how often it ran.
func fakePower(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	old, oldWait := powerActions[doneSleep], whenDoneCountdown
	powerActions[doneSleep] = func(*queueTab) error { n.Add(1); return errors.New("not here") }
	whenDoneCountdown = 60 * time.Millisecond
	t.Cleanup(func() { powerActions[doneSleep], whenDoneCountdown = old, oldWait })
	return &n
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// After the countdown the action runs once and the choice goes back to
// "do nothing": a choice made today doesn't act again at the next batch.
func TestWhenDoneActsAfterTheCountdown(t *testing.T) {
	ran := fakePower(t)
	q, eng, _ := newTestQueue(t, "a.bin")
	eng.CancelAll(false) // nothing left to do
	q.vm.Replace(eng.Jobs())
	q.whenDoneMenu()
	q.setWhenDone(doneSleep)
	q.startWhenDone()
	waitFor(t, func() bool { return ran.Load() == 1 })
	waitFor(t, func() bool { return q.whenDone == doneNothing })
	if !q.whenDoneItems[doneNothing].Checked || q.whenDoneItems[doneSleep].Checked {
		t.Error("the menu's tick didn't follow")
	}
	if got := q.vm.StatusLine(); !strings.Contains(got, "not here") {
		t.Errorf("the failure wasn't told: %q", got)
	}
}

// Cancel in the dialog: nothing happens, and the choice is gone.
func TestWhenDoneCancel(t *testing.T) {
	ran := fakePower(t)
	whenDoneCountdown = time.Second
	q, eng, _ := newTestQueue(t, "a.bin")
	eng.CancelAll(false)
	q.vm.Replace(eng.Jobs())
	q.whenDoneMenu()
	q.setWhenDone(doneSleep)
	q.startWhenDone()
	q.countdown.Hide() // what Cancel does
	time.Sleep(1200 * time.Millisecond)
	if ran.Load() != 0 {
		t.Error("the action ran after Cancel")
	}
	if q.whenDone != doneNothing {
		t.Error("the choice stayed after Cancel")
	}
}

// Links added during the countdown: not finished after all. The choice stays
// for when they are.
func TestWhenDoneWaitsForNewDownloads(t *testing.T) {
	ran := fakePower(t)
	whenDoneCountdown = 300 * time.Millisecond
	q, _, _ := newTestQueue(t, "a.bin") // a.bin stays queued: busy
	q.whenDoneMenu()
	q.setWhenDone(doneSleep)
	q.startWhenDone()
	time.Sleep(500 * time.Millisecond)
	if ran.Load() != 0 {
		t.Error("the action ran with a download still queued")
	}
	if q.whenDone != doneSleep {
		t.Error("the choice was dropped")
	}
}
