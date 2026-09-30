package main

import (
	"context"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// TestREADMEScreenshot draws the Download tab with sample jobs into
// docs/screenshot.png, off screen, so the README picture can be regenerated
// whenever the window changes. Skipped unless asked for:
//
//	SIPHON_SCREENSHOT=1 go test ./cmd/siphon-gui -run READMEScreenshot
func TestREADMEScreenshot(t *testing.T) {
	if os.Getenv("SIPHON_SCREENSHOT") == "" {
		t.Skip("set SIPHON_SCREENSHOT=1 to regenerate docs/screenshot.png")
	}
	a := test.NewTempApp(t)
	test.ApplyTheme(t, theme.DefaultTheme())

	cfg := site.SiteConfig{Name: "mega", MaxSegments: 8}.WithDefaults()
	eng, err := queue.New(queue.Options{Resolvers: []site.Resolver{shotResolver{}}, Configs: []site.SiteConfig{cfg}})
	if err != nil {
		t.Fatal(err)
	}

	const mb = 1 << 20
	now := time.Now()
	vm := newViewModel()
	jobs := []struct {
		name  string
		st    queue.State
		done  int64
		size  int64
		rate  float64
		conns int
	}{
		{"Holiday 2025 - Day 1.mp4", queue.StateRunning, 1310 * mb, 2150 * mb, 21.4 * mb, 8},
		{"Holiday 2025 - Day 2.mp4", queue.StateRunning, 420 * mb, 1870 * mb, 18.9 * mb, 8},
		{"project-backup-2026-09.zip", queue.StateRunning, 96 * mb, 640 * mb, 9.7 * mb, 3},
		{"ubuntu-24.04.3-desktop-amd64.iso", queue.StateRunning, 2890 * mb, 6010 * mb, 24.8 * mb, 4},
		{"Holiday 2025 - Day 3.mp4", queue.StateQueued, 0, 1990 * mb, 0, 0},
		{"Holiday 2025 - Day 4.mp4", queue.StateQueued, 0, 2320 * mb, 0, 0},
		{"lecture-notes.pdf", queue.StatePaused, 3 * mb, 11 * mb, 0, 0},
		{"soundtrack.flac", queue.StateDone, 412 * mb, 412 * mb, 0, 0},
		{"photos-album.tar", queue.StateDone, 1204 * mb, 1204 * mb, 0, 0},
		{"old-mirror.zip", queue.StateFailed, 0, 0, 0, 0},
	}
	vm.mu.Lock()
	for i, j := range jobs {
		job := queue.Job{ID: string(rune('a' + i)), Filename: j.name, State: j.st, Done: j.done, Size: j.size, Conns: j.conns,
			AddedAt: now.Add(-time.Duration(len(jobs)-i) * 7 * time.Minute)}
		if j.st == queue.StateFailed {
			job.Error = "HTTP 404: the file was removed from the server"
		}
		vm.apply(job, now)
		if j.rate > 0 {
			vm.rate[job.ID] = j.rate
		}
	}
	vm.mu.Unlock()

	w := test.NewTempWindow(t, container.NewStack())
	q, view := newQueueTab(w, a.Preferences(), eng, vm, nil)
	w.SetContent(view)
	w.Resize(fyne.NewSize(1100, 640))
	q.render()

	out := filepath.Join("..", "..", "docs", "screenshot.png")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, w.Canvas().Capture()); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", out)
}

// shotResolver is the minimum the engine needs to exist; nothing is resolved.
type shotResolver struct{}

func (shotResolver) Match(string) bool { return false }
func (shotResolver) Resolve(context.Context, string, func(site.Item) error) ([]site.ItemError, error) {
	return nil, nil
}
func (shotResolver) ResolveOne(context.Context, string) (site.Item, error) { return site.Item{}, nil }
func (shotResolver) Diagnose(context.Context) ([]site.LayerResult, error)  { return nil, nil }
