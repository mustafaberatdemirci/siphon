// Package testutil holds helpers shared between tests.
//
// It is only imported from _test.go files, so it never ends up in the
// production binary.
package testutil

import (
	"os"
	"testing"
	"time"
)

// TempDir is used instead of t.TempDir().
//
// On Windows, antivirus and the search indexer briefly hold on to freshly
// written files; t.TempDir()'s registered RemoveAll fails at that moment with
// "directory not empty" and the test shows FAIL even though every assertion
// in its body passed. The download tests chase exactly silent data
// corruption, so an environment-induced flake is especially expensive here:
// in a red suite nobody notices a real regression.
//
// The fix is to retry the removal. If it still fails the test is not failed,
// only a note is left: failing to delete a temp folder says nothing about the
// tool's behavior.
func TempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "siphon-test-")
	if err != nil {
		t.Fatalf("could not create temp folder: %v", err)
	}
	t.Cleanup(func() {
		const attempts = 25
		for i := 0; i < attempts; i++ {
			if rerr := os.RemoveAll(dir); rerr == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Logf("temp folder could not be deleted (Windows file lock), can be cleaned up by hand: %s", dir)
	})
	return dir
}
