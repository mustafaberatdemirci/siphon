package instance

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

// Two launches at the same moment: both used to find nobody listening yet
// and both became the instance. Whatever the timing, one does.
func TestSimultaneousLaunchesGiveOneInstance(t *testing.T) {
	for round := 0; round < 20; round++ {
		dir := testutil.TempDir(t)
		const launches = 8
		var wg sync.WaitGroup
		var instances atomic.Int32
		releases := make([]func(), launches)
		start := make(chan struct{})
		for i := 0; i < launches; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				release, running := Claim(dir, Handlers{})
				releases[i] = release
				if !running {
					instances.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if n := instances.Load(); n != 1 {
			t.Fatalf("round %d: %d of %d simultaneous launches became the instance", round, n, launches)
		}
		for _, r := range releases {
			r()
		}
	}
}

// The lock goes with the instance: the next launch takes it.
func TestLockIsFreedOnRelease(t *testing.T) {
	dir := testutil.TempDir(t)
	release, running := Claim(dir, Handlers{})
	if running {
		t.Fatal("the first launch found an instance")
	}
	if _, got, err := tryLock(filepath.Join(dir, lockName)); err != nil || got {
		t.Fatalf("the lock isn't held while the instance runs (got=%v, err=%v)", got, err)
	}
	release()
	unlock, got, err := tryLock(filepath.Join(dir, lockName))
	if err != nil || !got {
		t.Fatalf("the lock wasn't freed (got=%v, err=%v)", got, err)
	}
	unlock()
}

// A launch that finds the lock held, but nobody answering yet, doesn't start
// beside the instance that is still coming up.
func TestLaunchDuringAnotherStartDoesNotBecomeAnInstance(t *testing.T) {
	old := startWait
	startWait = 0
	defer func() { startWait = old }()
	dir := testutil.TempDir(t)
	unlock, got, err := tryLock(filepath.Join(dir, lockName)) // a launch between its lock and its listener
	if err != nil || !got {
		t.Fatalf("could not take the lock (got=%v, err=%v)", got, err)
	}
	defer unlock()
	if _, running := Claim(dir, Handlers{}); !running {
		t.Fatal("a launch became an instance while another held the lock")
	}
}
