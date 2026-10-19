package queue

import (
	"context"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/store"
)

// A file whose name had to change for the disk is in the ledger under the
// site's name, as the command line writes and looks it up: each recognizes
// the other's downloads. The queue used to key it by the name on disk.
func TestLedgerKnowsTheSitesName(t *testing.T) {
	const raw = "a:b*.bin" // not a name Windows takes
	f := newFakeSite(t, raw)
	h := newHarness(t, f, "", 1)
	if _, err := h.e.Add(context.Background(), "file://"+raw, h.out); err != nil {
		t.Fatal(err)
	}
	h.start()
	defer h.stop()
	h.waitAll(StateDone, 10*time.Second)
	j := h.e.Jobs()[0]
	if j.Filename == raw {
		t.Fatalf("the name %q wasn't changed for the disk; the test needs one that is", raw)
	}
	if j.Name != raw {
		t.Errorf("the job keeps the site's name as %q, want %q", j.Name, raw)
	}

	l, err := store.Open(h.out)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	e, ok := l.Lookup(j.Dir, j.SourcePage, raw)
	if !ok {
		t.Fatalf("the ledger doesn't know %q by the site's name", raw)
	}
	if e.Filename != raw {
		t.Errorf("the ledger entry's name is %q, want %q", e.Filename, raw)
	}
}

// A job of a queue file written before the site's name was kept takes it
// from its next resolution.
func TestOldJobTakesTheSitesNameFromResolution(t *testing.T) {
	const raw = "c:d*.bin"
	f := newFakeSite(t, raw)
	h := newHarness(t, f, "", 1)
	if _, err := h.e.Add(context.Background(), "file://"+raw, h.out); err != nil {
		t.Fatal(err)
	}
	h.e.mu.Lock()
	h.e.jobs[0].Name = "" // as loaded from an older queue file
	h.e.mu.Unlock()
	forgetItems(h.e)
	h.start()
	defer h.stop()
	h.waitAll(StateDone, 10*time.Second)
	if j := h.e.Jobs()[0]; j.Name != raw {
		t.Errorf("Name = %q, want %q", j.Name, raw)
	}
}
