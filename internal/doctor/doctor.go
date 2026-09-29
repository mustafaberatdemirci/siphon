// Package doctor runs the layer diagnosis and prints a readable report.
//
// This package is the tool's reason to exist. The real cost of writing a
// scraper isn't the time to write the code, it is seeing "0 files
// downloaded" six months later and not knowing why. doctor removes that
// guesswork: it tells within seconds which layer broke and saves the broken
// response to disk.
package doctor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// Named pairs a resolver with its name. site.Resolver doesn't carry its name.
type Named struct {
	Name     string
	Resolver site.Resolver
}

// Report is the diagnosis result of a single site.
type Report struct {
	Site    string
	Results []site.LayerResult
	// Err is set when Diagnose itself couldn't run (e.g. an empty canary list).
	Err error
}

// Worst returns the worst status in the report.
func (r Report) Worst() site.LayerStatus {
	if r.Err != nil {
		return site.StatusFail
	}
	worst := site.StatusOK
	for _, res := range r.Results {
		switch res.Status {
		case site.StatusFail:
			return site.StatusFail
		case site.StatusWarn:
			worst = site.StatusWarn
		}
	}
	return worst
}

// Run runs the diagnosis for each site.
//
// Sites run SEQUENTIALLY, not in parallel: doctor's output is for humans and
// interleaved lines make diagnosis harder. Also, hitting a rate limit during
// diagnosis would break the very thing being diagnosed.
func Run(ctx context.Context, sites []Named) []Report {
	out := make([]Report, 0, len(sites))
	for _, s := range sites {
		res, err := s.Resolver.Diagnose(ctx)
		out = append(out, Report{Site: s.Name, Results: res, Err: err})
	}
	return out
}

// Format writes the reports and returns the worst status.
func Format(w io.Writer, reports []Report) site.LayerStatus {
	worst := site.StatusOK
	for i, r := range reports {
		if i > 0 {
			fmt.Fprintln(w)
		}
		if s := r.Worst(); s == site.StatusFail {
			worst = site.StatusFail
		} else if s == site.StatusWarn && worst != site.StatusFail {
			worst = site.StatusWarn
		}

		fmt.Fprintf(w, "%s\n", r.Site)
		if r.Err != nil {
			fmt.Fprintf(w, "  %-10s %-5s %s\n", "-", site.StatusFail, r.Err)
			continue
		}
		if len(r.Results) == 0 {
			fmt.Fprintf(w, "  %-10s %-5s %s\n", "-", site.StatusWarn, "no diagnosis results")
			continue
		}
		for _, res := range r.Results {
			fmt.Fprintf(w, "  %-10s %-5s %s\n", res.Layer, res.Status, res.Detail)
			if res.Evidence != "" {
				fmt.Fprintf(w, "  %-10s %-5s   %s\n", "", "", res.Evidence)
			}
		}
	}
	return worst
}

// Recorder is the response recorder enabled by --record.
//
// Saved files are NOT copied into testdata AUTOMATICALLY. This is deliberate:
// a recording is a real response and may contain traces of content such as
// file names. A human decides what becomes a fixture; the distributed binary
// isn't inside the source tree either.
type Recorder struct {
	Dir string

	mu    sync.Mutex
	saved []string
	errs  []error
}

// DefaultDir is used when --record-dir isn't given.
const DefaultDir = "recordings"

// For produces a record function for a specific site.
func (r *Recorder) For(siteName string) func(name string, data []byte) {
	return func(name string, data []byte) {
		r.save(siteName, name, data)
	}
}

func (r *Recorder) save(siteName, name string, data []byte) {
	dir := r.Dir
	if dir == "" {
		dir = DefaultDir
	}
	// The timestamp prevents name collisions and tells which recording is
	// newer when diffing.
	stamp := time.Now().UTC().Format("20060102-150405")
	fname := fmt.Sprintf("%s-%s-%s", stamp, safe(siteName), safe(name))
	path := filepath.Join(dir, fname)

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %w", dir, err))
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %w", path, err))
		return
	}
	r.saved = append(r.saved, path)
}

// Saved returns the paths of the saved files.
func (r *Recorder) Saved() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.saved))
	copy(out, r.saved)
	sort.Strings(out)
	return out
}

// Errs returns the errors that happened while recording.
// A recording error does NOT fail the diagnosis: the real job is the layer
// report, recording is a helper.
func (r *Recorder) Errs() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]error, len(r.errs))
	copy(out, r.errs)
	return out
}

// safe does minimal cleanup for a file name. The full Windows cleanup is in
// dl.Component, but doctor shouldn't depend on it: recording names are short
// labels we produce ourselves, the site name and the file name.
func safe(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "record"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
