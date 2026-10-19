package dl

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// protectTestDirs points protectedDirs at exeDir (sites.toml and the tools)
// and toolDir (the tools only) for the rest of the test.
func protectTestDirs(t *testing.T, exeDir, toolDir string) {
	t.Helper()
	old := protectedDirs
	protectedDirs = func() (string, []string) { return exeDir, []string{exeDir, toolDir} }
	t.Cleanup(func() { protectedDirs = old })
}

func TestCheckProtected(t *testing.T) {
	exeDir, toolDir, other := tempDir(t), tempDir(t), tempDir(t)
	protectTestDirs(t, exeDir, toolDir)
	sep := string(filepath.Separator)
	for _, c := range []struct {
		path    string
		refused bool
	}{
		{filepath.Join(exeDir, "sites.toml"), true},
		{filepath.Join(exeDir, "SITES.TOML"), true},
		{filepath.Join(toolDir, "sites.toml"), false}, // only read next to the executable
		{filepath.Join(other, "sites.toml"), false},
		{filepath.Join(exeDir, "yt-dlp.exe"), true},
		{filepath.Join(toolDir, "YT-DLP.EXE"), true},
		{filepath.Join(toolDir, "ffmpeg"), true},
		{filepath.Join(toolDir, "deno.cmd"), true},
		{filepath.Join(toolDir, "ffprobe.bat"), true},
		{strings.ToUpper(toolDir) + sep + "gallery-dl.exe", true},
		{toolDir + sep + "." + sep + "gallery-dl", true},
		{filepath.Join(other, "yt-dlp.exe"), false},
		{filepath.Join(exeDir, "yt-dlp.mp4"), false},
		{filepath.Join(exeDir, "yt-dlp notes.txt"), false},
		{filepath.Join(exeDir, "gallery-dl.conf"), false},
	} {
		err := checkProtected(c.path)
		if refused := errors.Is(err, ErrProtectedName); refused != c.refused {
			t.Errorf("checkProtected(%s) = %v; refused want %v", c.path, err, c.refused)
		}
	}
}

// A refused name is refused before anything is requested or written, and
// the same name downloads as usual in any other folder.
func TestDownloadRefusesProtectedName(t *testing.T) {
	body := []byte("file content")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	out := tempDir(t)
	protectTestDirs(t, out, tempDir(t))
	d := &Downloader{Client: srv.Client()}
	it := site.Item{URL: srv.URL + "/f", SourcePage: "https://example.test/f", Filename: "yt-dlp.exe", Size: int64(len(body))}

	if _, err := d.Plan(out, it); !errors.Is(err, ErrProtectedName) {
		t.Fatalf("Plan: %v, want ErrProtectedName", err)
	}
	if _, err := d.Download(context.Background(), out, it); !errors.Is(err, ErrProtectedName) {
		t.Fatalf("Download: %v, want ErrProtectedName", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("%d requests were made for a refused name", n)
	}
	if entries := ownEntries(t, out); len(entries) != 0 {
		t.Errorf("files were written for a refused name: %v", entries)
	}

	// In a folder of its own (an album's) the same name is an ordinary file.
	it.Dir = "Album"
	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatalf("download into a subfolder: %v", err)
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, body) {
		t.Fatal("content differs")
	}
}
