package queue

import (
	"context"
	"strings"
	"testing"
	"time"
)

// orderedEngine is an engine with jobs a..e in that order, none running.
func orderedEngine(t *testing.T) *Engine {
	t.Helper()
	f := newFakeSite(t, "a", "b", "c", "d", "e")
	h := newHarness(t, f, "", 1)
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		if _, err := h.e.Add(context.Background(), "file://"+n, h.out); err != nil {
			t.Fatal(err)
		}
	}
	return h.e
}

func names(e *Engine) string {
	var out []string
	for _, j := range e.Jobs() {
		out = append(out, j.Filename)
	}
	return strings.Join(out, "")
}

func ids(e *Engine, names ...string) []string {
	var out []string
	for _, n := range names {
		out = append(out, jobByName(e, n).ID)
	}
	return out
}

func TestMoveToTopAndBottom(t *testing.T) {
	e := orderedEngine(t)
	e.MoveToTop(ids(e, "d", "b")) // their own order among themselves: b before d
	if got := names(e); got != "bdace" {
		t.Errorf("to top: %s, want bdace", got)
	}
	e.MoveToBottom(ids(e, "b"))
	if got := names(e); got != "daceb" {
		t.Errorf("to bottom: %s, want daceb", got)
	}
}

// Arrange moves jobs among their own places: the others stay put.
func TestArrangeKeepsTheOthersInPlace(t *testing.T) {
	e := orderedEngine(t)
	e.Arrange(ids(e, "d", "b")) // b and d trade places; a, c, e stay
	if got := names(e); got != "adcbe" {
		t.Errorf("got %s, want adcbe", got)
	}
	e.Arrange(append(ids(e, "e", "a"), "unknown", ids(e, "e")[0]))
	if got := names(e); got != "edcba" {
		t.Errorf("got %s, want edcba", got)
	}
}

// The order is the order jobs start in, and it survives a restart.
func TestOrderDecidesWhatStartsNext(t *testing.T) {
	f := newFakeSite(t, "a", "b", "c")
	f.delay = 50 * time.Millisecond
	state := t.TempDir() + "/queue.json"
	h := newHarness(t, f, state, 1)
	for _, n := range []string{"a", "b", "c"} {
		if _, err := h.e.Add(context.Background(), "file://"+n, h.out); err != nil {
			t.Fatal(err)
		}
	}
	h.e.MoveToTop(ids(h.e, "c"))
	h.e.flush()
	again, err := New(Options{StatePath: state, Resolvers: h.e.resolvers, Configs: h.e.cfgs, Client: f.srv.Client(), MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(again); got != "cab" {
		t.Errorf("after a restart: %s, want cab", got)
	}

	h.start()
	defer h.stop()
	h.waitAll(StateDone, 10*time.Second)
	var started []string
	h.mu.Lock()
	for _, ev := range h.events {
		if ev.State == StateRunning && (len(started) == 0 || started[len(started)-1] != ev.Filename) {
			started = append(started, ev.Filename)
		}
	}
	h.mu.Unlock()
	if strings.Join(started, "") != "cab" {
		t.Errorf("started in the order %v, want c, a, b", started)
	}
}
