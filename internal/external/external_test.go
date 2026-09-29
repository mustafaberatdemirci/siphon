package external

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

// fakeTools: yt-dlp and gallery-dl played by the test binary; every call's
// arguments go to the returned log.
func fakeTools(t *testing.T) (Tools, func() [][]string) {
	t.Helper()
	exe := testutil.FakeToolPath(t)
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv(testutil.FakeToolLogEnv, log)
	calls := func() [][]string {
		b, _ := os.ReadFile(log)
		var out [][]string
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var args []string
			if json.Unmarshal([]byte(line), &args) == nil {
				out = append(out, args)
			}
		}
		return out
	}
	return Tools{YtDlp: exe, GalleryDL: exe}, calls
}

func TestYtDlpProbe(t *testing.T) {
	tools, _ := fakeTools(t)
	ctx := context.Background()

	pl, entries, err := tools.YtDlpProbe(ctx, "https://v.test/watch?v=one")
	if err != nil || pl != "" || len(entries) != 1 {
		t.Fatalf("video: %q %+v %v", pl, entries, err)
	}
	if e := entries[0]; e.Title != "Video one" || e.ID != "one" || e.Size != 1000 || e.URL != "https://v.test/watch?v=one" {
		t.Errorf("video entry: %+v", e)
	}

	pl, entries, err = tools.YtDlpProbe(ctx, "https://v.test/playlist")
	if err != nil || pl != "My list" || len(entries) != 2 || entries[1].URL != "https://v.test/watch?v=two" {
		t.Fatalf("playlist: %q %+v %v", pl, entries, err)
	}
	if entries[0].Size != -1 {
		t.Errorf("a size was made up for a flat entry: %d", entries[0].Size)
	}

	if _, _, err := tools.YtDlpProbe(ctx, "https://v.test/about"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("unknown page: %v, want ErrUnsupported", err)
	}
	// A site yt-dlp knows but can't get: its own reason, not "unsupported".
	_, _, err = tools.YtDlpProbe(ctx, "https://v.test/private")
	if err == nil || errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "Private video") {
		t.Errorf("private video: %v", err)
	}

	if _, _, err := (Tools{}).YtDlpProbe(ctx, "https://v.test/watch?v=one"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("without yt-dlp: %v", err)
	}
}

func TestYtDlpDownload(t *testing.T) {
	tools, calls := fakeTools(t)
	dir := t.TempDir()
	var reports [][2]int64
	path, size, err := tools.YtDlpDownload(context.Background(), "https://v.test/watch?v=one", dir,
		func(done, total int64) { reports = append(reports, [2]int64{done, total}) })
	if err != nil {
		t.Fatal(err)
	}
	fi, serr := os.Stat(path)
	if serr != nil || fi.Size() != size || filepath.Dir(path) != dir || filepath.Base(path) != "Video one [one].mp4" {
		t.Fatalf("got %s (%d bytes): %v", path, size, serr)
	}
	if len(reports) != 3 || reports[2][0] != size || reports[2][1] != size {
		t.Errorf("progress reports %v", reports)
	}

	// Without ffmpeg: the best single file; with it: yt-dlp is told where it is.
	args := strings.Join(calls()[0], " ")
	if !strings.Contains(args, "-f b ") || strings.Contains(args, "--ffmpeg-location") {
		t.Errorf("without ffmpeg the call was: %s", args)
	}
	tools.FFmpeg = `C:\tools\ffmpeg.exe`
	if _, _, err := tools.YtDlpDownload(context.Background(), "https://v.test/watch?v=two", dir, nil); err != nil {
		t.Fatal(err)
	}
	args = strings.Join(calls()[1], " ")
	if strings.Contains(args, "-f b ") || !strings.Contains(args, `--ffmpeg-location C:\tools\ffmpeg.exe`) {
		t.Errorf("with ffmpeg the call was: %s", args)
	}
	// The link always comes after "--": a link starting with "-" must not
	// become an option.
	if c := calls()[0]; len(c) < 2 || c[len(c)-2] != "--" {
		t.Errorf("the link isn't separated from the options: %v", c)
	}
}

// A file reported but not on disk (picture and sound left unmerged) is an
// error, never a success.
func TestYtDlpDownloadReportedFileMustExist(t *testing.T) {
	tools, _ := fakeTools(t)
	_, _, err := tools.YtDlpDownload(context.Background(), "https://v.test/split", t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "isn't on disk") || !strings.Contains(err.Error(), "ffmpeg") {
		t.Fatalf("got %v", err)
	}
}

// Canceling (pause, cancel, quit) ends the run with the context's error.
func TestYtDlpDownloadCanceled(t *testing.T) {
	tools, _ := fakeTools(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := tools.YtDlpDownload(ctx, "https://v.test/watch?v=one", t.TempDir(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestGalleryDL(t *testing.T) {
	tools, _ := fakeTools(t)
	ctx := context.Background()
	if err := tools.GalleryDLProbe(ctx, "https://p.test/album"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := tools.GalleryDLProbe(ctx, "https://p.test/other"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("unknown link: %v, want ErrUnsupported", err)
	}

	base := t.TempDir()
	var last int64
	dir, size, err := tools.GalleryDLDownload(ctx, "https://p.test/album", base, func(done, total int64) {
		if total != -1 {
			t.Errorf("a gallery total was made up: %d", total)
		}
		last = done
	})
	want := filepath.Join(base, "pics", "album")
	if err != nil || dir != want || size != int64(len("image a.jpg")+len("image b.jpg")) || last != size {
		t.Fatalf("download: %q %d (last report %d) %v", dir, size, last, err)
	}
	// Again: gallery-dl skips what is there, and the files still count.
	dir2, size2, err := tools.GalleryDLDownload(ctx, "https://p.test/album", base, nil)
	if err != nil || dir2 != dir || size2 != size {
		t.Errorf("second run: %q %d %v", dir2, size2, err)
	}
	if _, _, err := tools.GalleryDLDownload(ctx, "https://p.test/other", base, nil); err == nil || !strings.Contains(err.Error(), "Unsupported") {
		t.Errorf("unknown link: %v", err)
	}
}

// A configured path is the only place looked at: a wrong setting shows as
// "not installed" instead of quietly using another copy.
func TestFindConfiguredPath(t *testing.T) {
	exe, _ := os.Executable()
	got := Find(map[string]string{"yt_dlp": exe, "gallery_dl": filepath.Join(t.TempDir(), "missing.exe")})
	if got.YtDlp != exe || got.GalleryDL != "" {
		t.Errorf("Find = %+v", got)
	}
}

func TestParseProgress(t *testing.T) {
	for in, want := range map[string][2]int64{
		"1024 4096 NA":     {1024, 4096},
		"1024 NA 5000.5":   {1024, 5000},
		"1024 NA NA":       {1024, -1},
		"NA NA NA":         {-1, -1},
		"2048.0 4096.0 NA": {2048, 4096},
	} {
		if d, tot := parseProgress(in); d != want[0] || tot != want[1] {
			t.Errorf("parseProgress(%q) = %d, %d; want %v", in, d, tot, want)
		}
	}
}
