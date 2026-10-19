// Package fsx has file operations that hold up on Windows.
//
// Windows refuses to rename over a file that another program has open, and
// such programs come and go: an antivirus scanning a file it just saw
// closed, the search indexer, a backup tool. A file deleted a moment ago can
// also be "delete pending" until the last of them lets go. The refusal lasts
// milliseconds, but it failed a download whose last step is renaming the
// finished .part (measured with Kaspersky on: the scan on close), and lost a
// saved state now and then. Go's own tools retry the same way
// (cmd/go/internal/robustio).
package fsx

import (
	"os"
	"time"
)

// retryFor is how long a refused rename is retried.
const retryFor = 2 * time.Second

// Rename is os.Rename, retried for a while when Windows says another
// program has the file open.
func Rename(oldpath, newpath string) error {
	return retry(func() error { return os.Rename(oldpath, newpath) })
}

func retry(op func() error) error {
	var (
		err      error
		deadline time.Time
		wait     = 5 * time.Millisecond
	)
	for {
		err = op()
		if err == nil || !transient(err) {
			return err
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(retryFor)
		} else if time.Now().After(deadline) {
			return err
		}
		time.Sleep(wait)
		if wait < 200*time.Millisecond {
			wait *= 2
		}
	}
}
