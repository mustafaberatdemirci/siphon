// Package store keeps an append-only ledger of completed downloads.
//
// The ledger file lives UNDER THE OUTPUT ROOT, not next to the exe or in the
// cwd. Reason: otherwise a second run with a different -out would say
// "everything is already downloaded" and return 0 without downloading
// anything, even though the files aren't there. That would directly violate
// the tool's "no silent failure" principle. With the ledger next to the
// output, moving the output moves the ledger too.
package store

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileName is the name of the ledger file.
const FileName = "done.jsonl"

// Entry is one line of done.jsonl.
type Entry struct {
	// Key parts. Dir and Filename are RAW values (as given by the resolver),
	// not the sanitized form on disk: the key must be computable before the
	// download starts.
	SourcePage string `json:"source_page"`
	Dir        string `json:"dir"`
	Filename   string `json:"filename"`

	// Path is the real path RELATIVE to the output root. The original schema
	// didn't include it, but without it "the ledger says downloaded, but where
	// is the file" cannot be answered: if the user deletes the file, trusting
	// the ledger and skipping would be a silent failure. It is kept relative
	// so the ledger stays valid when the output folder is moved.
	Path string `json:"path"`

	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	TS     string `json:"ts"`
}

// Key produces an item's ledger key.
//
// The key does NOT include Item.URL. bunkr's signed CDN address changes on
// every run (the XOR key is tied to an hourly window); keying by URL would
// break idempotent restarts every time.
//
// Fields are mixed with a length prefix: plain concatenation ("a"+"bc" vs
// "ab"+"c") could map two different items to the same key, and file names can
// contain any character, so there is no safe separator.
func Key(dir, sourcePage, filename string) string {
	h := sha256.New()
	for _, s := range []string{dir, sourcePage, filename} {
		fmt.Fprintf(h, "%d:%s", len(s), s)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// Ledger is the append-only ledger file.
type Ledger struct {
	path string

	mu      sync.Mutex
	done    map[string]Entry
	f       *os.File
	skipped int
}

// Open reads the ledger under the output root and opens it for appending.
// If the file doesn't exist it starts with an empty ledger.
func Open(outRoot string) (*Ledger, error) {
	if err := os.MkdirAll(outRoot, 0o755); err != nil {
		return nil, fmt.Errorf("could not create output root: %w", err)
	}
	path := filepath.Join(outRoot, FileName)

	l := &Ledger{path: path, done: map[string]Entry{}}
	if err := l.load(); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("could not open %s: %w", path, err)
	}
	// A last line left half-written by a crash has no line ending. If it is
	// not terminated, the first new entry is glued onto it, the two together
	// become one corrupt line and the good entry is lost on the next open.
	if err := terminateLastLine(path, f); err != nil {
		f.Close()
		return nil, fmt.Errorf("could not repair %s: %w", path, err)
	}
	l.f = f
	return l, nil
}

// terminateLastLine appends a '\n' if the file is not empty and doesn't end
// with one.
func terminateLastLine(path string, w *os.File) error {
	r, err := os.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()
	fi, err := r.Stat()
	if err != nil {
		return err
	}
	if fi.Size() == 0 {
		return nil
	}
	last := make([]byte, 1)
	if _, err := r.ReadAt(last, fi.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	_, err = w.Write([]byte{'\n'})
	return err
}

func (l *Ledger) load() error {
	f, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("could not read %s: %w", l.path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Long file names and paths can exceed the default 64 KB limit.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(line, &e) != nil || e.Filename == "" {
			// A last line half-written during a crash is an expected state.
			// Treating the ledger as corrupt and failing the run would turn a
			// recoverable state into a fatal one. But it doesn't pass
			// silently either: it is counted.
			l.skipped++
			continue
		}
		l.done[Key(e.Dir, e.SourcePage, e.Filename)] = e
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("could not scan %s: %w", l.path, err)
	}
	return nil
}

// Lookup returns the item's entry if it was downloaded before.
func (l *Ledger) Lookup(dir, sourcePage, filename string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.done[Key(dir, sourcePage, filename)]
	return e, ok
}

// Add appends a new line and writes it to disk.
//
// Every line is fsynced: downloaded files are gigabyte-sized, one fsync per
// file is an immeasurably small cost. In return, after a power cut the
// ledger stays consistent with the downloaded files.
func (l *Ledger) Add(e Entry) error {
	if e.Filename == "" {
		return errors.New("a file name is required for a ledger entry")
	}
	if e.TS == "" {
		e.TS = time.Now().UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return errors.New("ledger is closed")
	}
	if _, err := l.f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("could not write %s: %w", l.path, err)
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("could not sync %s: %w", l.path, err)
	}
	l.done[Key(e.Dir, e.SourcePage, e.Filename)] = e
	return nil
}

// Len is the number of unique items in the ledger.
func (l *Ledger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.done)
}

// Skipped is the number of unreadable lines. If non-zero, the user should be told.
func (l *Ledger) Skipped() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.skipped
}

// Path is the path of the ledger file.
func (l *Ledger) Path() string { return l.path }

func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
