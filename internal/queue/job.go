// Package queue drives a persistent download queue: jobs start, pause and
// resume one by one; the list stays in place when the app is closed and
// reopened.
//
// Why a separate package: the run package is the right model for a batch
// run (command line: give a URL, wait until it's done). The window, however,
// wants an IDM-style queue: add a link, it gets queued, pause whatever you
// want. Both models use the SAME code (run.Worker) to download a single item;
// only scheduling and lifecycle differ.
package queue

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// State is where a job is in its lifecycle.
type State string

const (
	StateQueued  State = "queued"  // queued, waiting to start
	StateRunning State = "running" // downloading
	StatePaused  State = "paused"  // stopped by the user; the .part is in place, can be resumed
	StateDone    State = "done"    // downloaded and recorded
	StateFailed  State = "failed"  // permanent error; resume can be tried
	StateSkipped State = "skipped" // the ledger already had it, the file is in place
	StateStopped State = "stopped" // captcha: user action required
	StateWaiting State = "waiting" // the site's quota ran out; retried automatically at RetryAt
)

// Active reports whether the job is consuming resources right now.
func (s State) Active() bool { return s == StateRunning }

// Finished reports that the job will never start by itself again.
func (s State) Finished() bool { return s == StateDone || s == StateSkipped }

// Resumable covers the states where the user can say "resume". Waiting is
// here too: if the user changed IP they don't want to wait for the reset time,
// they press ▶.
func (s State) Resumable() bool {
	return s == StatePaused || s == StateFailed || s == StateStopped || s == StateWaiting
}

// Job is a single file in the queue. It is written to disk as JSON, so it
// only carries information that can be rebuilt: the download URL and secret
// material (the mega key) are NOT HERE, they are re-resolved from
// SourcePage. On mega the link carries the key, so SourcePage itself is
// sensitive; the queue file lives in the user's own config folder.
type Job struct {
	ID         string `json:"id"`
	Site       string `json:"site"`
	SourcePage string `json:"source_page"`
	OutDir     string `json:"out_dir"`
	Dir        string `json:"dir"`
	// Filename is for display and name ownership. After the first Plan it is
	// the FINAL name assigned by the downloader ("(2)" suffix included); it is
	// the guarantee of continuing under the same name when the app is closed
	// and reopened.
	Filename string `json:"filename"`
	Path     string `json:"path,omitempty"` // final path (Plan / Download)
	Size     int64  `json:"size"`
	Index    int    `json:"index"`

	State      State     `json:"state"`
	Done       int64     `json:"done"` // last known downloaded bytes
	Error      string    `json:"error,omitempty"`
	AddedAt    time.Time `json:"added_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	// RetryAt is when a job in the Waiting state returns to the queue by
	// itself. Persistent: the hold stays in place even if the app is closed
	// and reopened.
	RetryAt time.Time `json:"retry_at,omitempty"`

	// Conns is how many connections a running job is being fetched over
	// right now; 0 when unknown or not running. Runtime only.
	Conns int `json:"-"`
}

// jobID is the job's stable identity: if the same file is added to the same
// folder a second time it gets the same id and isn't duplicated in the queue.
func jobID(outDir, sourcePage, dir, filename string) string {
	h := sha256.New()
	for _, part := range []string{outDir, sourcePage, dir, filename} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
