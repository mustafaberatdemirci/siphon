package external

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Against the REAL tools and the network; skipped unless SIPHON_TOOLS_DIR
// points to a folder with yt-dlp and gallery-dl in it:
//
//	SIPHON_TOOLS_DIR=/path/to/tools go test ./internal/external -run Live -v
//
// The links are the tools' own test fixtures: YouTube's first video (19 s)
// and a test image on imgur.
func liveTools(t *testing.T) Tools {
	dir := os.Getenv("SIPHON_TOOLS_DIR")
	if dir == "" {
		t.Skip("set SIPHON_TOOLS_DIR to run against the real tools")
	}
	find := func(name string) string {
		for _, n := range []string{name + ".exe", name} {
			if p := filepath.Join(dir, n); isFile(p) {
				return p
			}
		}
		return ""
	}
	return Tools{YtDlp: find("yt-dlp"), GalleryDL: find("gallery-dl"), FFmpeg: find("ffmpeg")}
}

func TestLiveYtDlp(t *testing.T) {
	tools := liveTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	pl, entries, err := tools.YtDlpProbe(ctx, "https://www.youtube.com/watch?v=jNQXAC9IVRw")
	if err != nil || pl != "" || len(entries) != 1 || entries[0].Title != "Me at the zoo" {
		t.Fatalf("probe: %q %+v %v", pl, entries, err)
	}
	if _, _, err := tools.YtDlpProbe(ctx, "https://example.com/"); err != ErrUnsupported {
		t.Errorf("example.com: %v, want ErrUnsupported", err)
	}

	dir := t.TempDir()
	if tools.FFmpeg == "" {
		// Without ffmpeg a video with separate picture and sound must fail
		// with a reason, not hand over the sound alone.
		_, _, err := tools.YtDlpDownload(ctx, entries[0].URL, dir, nil)
		if err == nil || !strings.Contains(err.Error(), "ffmpeg") {
			t.Errorf("video without ffmpeg: %v, want an error naming ffmpeg", err)
		}
	}

	// An audio site needs no merging.
	const track = "https://soundcloud.com/ethmusic/lostin-powers-she-so-heavy"
	var calls int
	path, size, err := tools.YtDlpDownload(ctx, track, dir, func(done, total int64) { calls++ })
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if fi, serr := os.Stat(path); serr != nil || fi.Size() != size || size == 0 {
		t.Errorf("reported %s (%d bytes): %v", path, size, serr)
	}
	if calls == 0 {
		t.Error("no progress reports")
	}
	t.Logf("%s, %d bytes, %d progress reports", filepath.Base(path), size, calls)

	// Again: already on disk.
	path2, _, err := tools.YtDlpDownload(ctx, track, dir, nil)
	if err != nil || path2 != path {
		t.Errorf("second run: %q %v", path2, err)
	}
}

func TestLiveGalleryDL(t *testing.T) {
	tools := liveTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := tools.GalleryDLProbe(ctx, "https://imgur.com/21yMxCS"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := tools.GalleryDLProbe(ctx, "https://example.com/"); err != ErrUnsupported {
		t.Errorf("example.com: %v, want ErrUnsupported", err)
	}
	base := t.TempDir()
	dir, size, err := tools.GalleryDLDownload(ctx, "https://imgur.com/21yMxCS", base, nil)
	if err != nil || size == 0 || !strings.HasPrefix(dir, base) {
		t.Fatalf("download: %q %d %v", dir, size, err)
	}
	// Again: gallery-dl skips it, and it still counts.
	dir2, size2, err := tools.GalleryDLDownload(ctx, "https://imgur.com/21yMxCS", base, nil)
	if err != nil || dir2 != dir || size2 != size {
		t.Errorf("second run: %q %d %v", dir2, size2, err)
	}
	t.Logf("%s, %d bytes", dir, size)
}

// Installs the real packages for this system from their official releases
// into SIPHON_INSTALL_DIR, then checks that yt-dlp with ffmpeg merges a
// YouTube video into one file. Downloads ~280 MB on Windows; opt-in:
//
//	SIPHON_INSTALL_DIR=/tmp/tools go test ./internal/external -run LiveInstall -v -timeout 30m
func TestLiveInstall(t *testing.T) {
	dir := os.Getenv("SIPHON_INSTALL_DIR")
	if dir == "" {
		t.Skip("set SIPHON_INSTALL_DIR to install the real tools there")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	client := &http.Client{}
	for _, p := range Packages(runtime.GOOS, runtime.GOARCH) {
		if err := p.Resolve(ctx, client); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		start := time.Now()
		if err := Install(ctx, client, dir, p, nil); err != nil {
			t.Fatalf("install: %v", err)
		}
		t.Logf("%-10s %6.1f MB  sha256 %s…  %s", p.Tool, float64(p.Size)/(1<<20), p.SHA256[:12], time.Since(start).Round(time.Second))
	}
	find := func(name string) string {
		for _, n := range []string{name + ".exe", name} {
			if p := filepath.Join(dir, n); isFile(p) {
				return p
			}
		}
		return ""
	}
	tools := Tools{YtDlp: find("yt-dlp"), GalleryDL: find("gallery-dl"), FFmpeg: find("ffmpeg"), Deno: find("deno")}
	if tools.FFmpeg == "" {
		t.Skip("no ffmpeg package on this system; the merge check needs it")
	}
	out := t.TempDir()
	path, size, err := tools.YtDlpDownload(ctx, "https://www.youtube.com/watch?v=jNQXAC9IVRw", out, nil)
	if err != nil {
		t.Fatalf("download with ffmpeg: %v", err)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 1 {
		t.Errorf("%d files in the folder, want the single merged one", len(entries))
	}
	t.Logf("merged: %s (%d bytes)", filepath.Base(path), size)
}
