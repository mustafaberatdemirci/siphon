package queue

// Unpacking finished archives (internal/extract): a download that is an
// archive is unpacked next to itself once it is done; an archive in parts
// once every part of it in the queue is. It runs beside the downloads,
// holding no slot of the queue, and says what it did on the status line.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mustafaberatdemirci/siphon/internal/external"
	"github.com/mustafaberatdemirci/siphon/internal/extract"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// SetUnpack turns unpacking finished archives on or off, and whether the
// archive (every part of it) is deleted once unpacked. It applies to the
// downloads that finish from now on.
func (e *Engine) SetUnpack(on, deleteAfter bool) {
	e.mu.Lock()
	e.unpackOn, e.unpackDelete = on, deleteAfter
	e.mu.Unlock()
}

// unpackJob, under the lock: what to unpack now that j is done; ok is false
// when nothing is (unpacking off, not an archive, parts still coming, or
// unpacked already). parts are every file of the archive, first among them.
func (e *Engine) unpackJob(j *Job) (first string, parts []string, ok bool) {
	if !e.unpackOn || j.Path == "" {
		return "", nil, false
	}
	kind, part := extract.Of(filepath.Base(j.Path))
	if kind == extract.None {
		return "", nil, false
	}
	first, parts = j.Path, []string{j.Path}
	if part.Set != "" {
		first, parts = "", nil
		for _, o := range e.jobs {
			if o.OutDir != j.OutDir || o.Dir != j.Dir || o.Path == "" {
				continue
			}
			if k, p := extract.Of(filepath.Base(o.Path)); k == extract.None || p.Set != part.Set {
				continue
			}
			if o.State != StateDone {
				return "", nil, false // a part still to come; it unpacks the set when done
			}
			parts = append(parts, o.Path)
			if _, p := extract.Of(filepath.Base(o.Path)); p.Number == 1 {
				first = o.Path
			}
		}
		if first == "" {
			return "", nil, false // the first part isn't in the queue
		}
	}
	key := filepath.Clean(first)
	if e.unpacked[key] {
		return "", nil, false
	}
	e.unpacked[key] = true
	return first, parts, true
}

// sevenZip is 7-Zip's program, found the way the other tools are (the
// "direct" entry of sites.toml may name it).
func (e *Engine) sevenZip() string {
	for _, c := range e.cfgs {
		if c.Name == site.DirectName {
			return external.Find(c.Extra).SevenZip
		}
	}
	return external.Find(nil).SevenZip
}

// unpack unpacks a finished archive and says how it went. Run in its own
// goroutine.
func (e *Engine) unpack(first string, parts []string, source string, deleteAfter bool) {
	e.mu.Lock()
	ctx := e.runCtx
	e.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	name := filepath.Base(first)
	e.notice(fmt.Sprintf("Unpacking %s…", name))
	dest, err := extract.Extract(ctx, first, extract.Options{SevenZip: e.sevenZip(), Source: source})
	switch {
	case errors.Is(err, context.Canceled):
		return
	case errors.Is(err, extract.ErrNeeds7Zip):
		e.notice(fmt.Sprintf("%s wasn't unpacked: 7z, rar and archives in parts need 7-Zip installed.", name))
		return
	case err != nil:
		e.notice(fmt.Sprintf("Could not unpack %s: %v", name, err))
		return
	}
	if deleteAfter {
		for _, p := range parts {
			if err := os.Remove(p); err != nil {
				e.opt.Events.Errorf("could not delete %s after unpacking it: %v", p, err)
			}
		}
	}
	e.notice(fmt.Sprintf("Unpacked %s into %s.", name, filepath.Base(dest)))
}
