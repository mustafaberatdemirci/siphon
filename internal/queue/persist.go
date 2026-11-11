package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/fsx"
)

// stateVersion is the format version of the queue file. An unknown version
// isn't read: starting with an empty queue and saying so beats silently
// misinterpreting it.
const stateVersion = 1

type stateFile struct {
	Version int    `json:"version"`
	Jobs    []*Job `json:"jobs"`
}

// load reads the queue file. If the file doesn't exist: empty list and nil error.
func load(path string) ([]*Job, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not read the queue: %w", err)
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("the queue file is corrupt: %w", err)
	}
	if sf.Version != stateVersion {
		return nil, fmt.Errorf("the queue file is version %d, this version expects %d", sf.Version, stateVersion)
	}
	// Jobs that were "running" at shutdown MUST CONTINUE where they left off:
	// the user didn't stop them, the app closed. They are put back in the queue.
	for _, j := range sf.Jobs {
		if j.State == StateRunning {
			j.State = StateQueued
		}
	}
	return sf.Jobs, nil
}

// setAside renames a queue file that couldn't be read to
// "<name>.unreadable-<time>" next to it and returns the new path; "" if it
// couldn't be moved (it then stays where it is until the next save).
func setAside(path string) string {
	kept := path + ".unreadable-" + time.Now().Format("20060102-150405")
	if err := fsx.Rename(path, kept); err != nil {
		return ""
	}
	return kept
}

// UnreadableStateFile is where the queue file was kept when it couldn't be
// read at start (the queue then started empty); "" otherwise. The window
// tells the user: the log alone is not where anyone looks for a lost list.
func (e *Engine) UnreadableStateFile() string { return e.unreadable }

// save writes the queue ATOMICALLY: temp file + rename. A half-written queue
// file would lose the whole list on the next launch.
func save(path string, jobs []*Job) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(stateFile{Version: stateVersion, Jobs: jobs}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return fsx.Rename(tmp, path)
}

// DefaultStatePath is the default location of the queue file: the user's
// config folder (%AppData%\Siphon on Windows). It isn't written to the output
// folder: the queue is app-level, jobs can go to different output folders.
func DefaultStatePath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "Siphon", "queue.json"), nil
}
