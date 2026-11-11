package queue

import (
	"context"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// poolCfg is a site whose "localhost" file servers give one file at a time
// while its others give several: bunkr's *.scdn.st next to its *.cdn.cr.
func poolCfg() site.SiteConfig {
	return site.SiteConfig{
		Name: "fake", MaxConcurrent: 8, MaxRetries: 2,
		HostLimits: map[string]int{"localhost": 1},
	}.WithDefaults()
}

func (f *fakeSite) hostMax(host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.perHostMax[host]
}

// WHAT THE USER WOULD EXPERIENCE: an album whose files lie on two kinds of
// servers, one kind allowing a single connection. The whole site was held to
// one file at a time for that kind's sake. The limited pool's files must
// take turns while the other servers' files download beside them, and a
// file of the full pool must not sit in a queue slot showing 0%.
func TestHostPoolLimitsOnlyItsOwnFiles(t *testing.T) {
	f := newFakeSite(t, "fast-a.bin", "fast-b.bin", "pool-a.bin", "pool-b.bin", "pool-c.bin")
	h := newHarnessWith(t, f, "", 4, poolCfg())
	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	holds := map[string]chan struct{}{}
	for name := range f.files {
		holds[name] = f.holdNext(name)
	}
	release := func() {
		for name, ch := range holds {
			close(ch)
			delete(holds, name)
		}
	}
	t.Cleanup(release) // a held answer would keep the server from closing
	h.start()
	defer h.stop()

	// Queue order is fast-a, fast-b, pool-a, pool-b, pool-c; four may run.
	for _, name := range []string{"fast-a.bin", "fast-b.bin", "pool-a.bin"} {
		h.waitState(jobByName(h.e, name).ID, StateRunning, 10*time.Second)
	}
	time.Sleep(200 * time.Millisecond) // time for a fourth job to start, if one wrongly could
	for _, name := range []string{"pool-b.bin", "pool-c.bin"} {
		j := jobByName(h.e, name)
		if j.State != StateQueued || h.sawState(j.ID, StateRunning) {
			t.Errorf("%s is %s (ran: %v) while its pool's one slot is taken", name, j.State, h.sawState(j.ID, StateRunning))
		}
	}
	release()
	h.waitAll(StateDone, 20*time.Second)
	if got := f.hostMax("localhost"); got != 1 {
		t.Errorf("%d files of the limited pool were served at once, want 1", got)
	}
	if got := f.hostMax("127.0.0.1"); got < 2 {
		t.Errorf("the other servers' files never downloaded side by side (most at once: %d)", got)
	}
	for _, j := range h.e.Jobs() {
		if h.sawState(j.ID, StateFailed) {
			t.Errorf("%s failed on the way", j.Filename)
		}
	}
}

// After a restart the links aren't resolved, so a job's host is only known
// once it has started. One that finds its pool full goes back in line with
// its link kept (and is passed over until there is room) instead of waiting
// inside the worker.
func TestHostPoolWithUnresolvedLinks(t *testing.T) {
	f := newFakeSite(t, "pool-a.bin", "pool-b.bin", "pool-c.bin", "zfast-a.bin", "zfast-b.bin")
	f.delay = 150 * time.Millisecond
	h := newHarnessWith(t, f, "", 3, poolCfg())
	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	forgetItems(h.e)
	h.start()
	defer h.stop()

	h.waitAll(StateDone, 30*time.Second)
	if got := f.hostMax("localhost"); got != 1 {
		t.Errorf("%d files of the limited pool were served at once, want 1", got)
	}
	// The pool's files are first in line and three jobs may run: the two
	// that found the pool full made room for the other servers' files.
	if got := f.hostMax("127.0.0.1"); got < 2 {
		t.Errorf("files of the other servers waited behind the full pool (most at once: %d)", got)
	}
	r := h.e.workers["fake"].Resolver.(*fakeResolver)
	if got := r.resolveOneCount(); got != 5 {
		t.Errorf("%d resolutions for 5 files: a job sent back in line must keep its link", got)
	}
	for _, j := range h.e.Jobs() {
		if h.sawState(j.ID, StateFailed) {
			t.Errorf("%s failed on the way", j.Filename)
		}
	}
}
