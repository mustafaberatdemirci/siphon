package external

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releaseServer plays GitHub's (digest) or Codeberg's (SHA256SUMS) release
// API and serves the assets.
type releaseServer struct {
	srv    *httptest.Server
	assets map[string][]byte
	digest map[string]string // asset -> "sha256:…" (GitHub style); absent: none
}

func newReleaseServer(t *testing.T) *releaseServer {
	t.Helper()
	rs := &releaseServer{assets: map[string][]byte{}, digest: map[string]string{}}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/latest" {
			if r.Header.Get("User-Agent") == "" {
				http.Error(w, "User-Agent required", http.StatusForbidden) // as GitHub does
				return
			}
			type asset struct {
				Name   string `json:"name"`
				Size   int    `json:"size"`
				URL    string `json:"browser_download_url"`
				Digest string `json:"digest,omitempty"`
			}
			var list []asset
			for name, b := range rs.assets {
				list = append(list, asset{name, len(b), rs.srv.URL + "/dl/" + name, rs.digest[name]})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1", "assets": list})
			return
		}
		b, ok := rs.assets[strings.TrimPrefix(r.URL.Path, "/dl/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// add serves an asset with a GitHub-style digest.
func (rs *releaseServer) add(name string, b []byte) {
	rs.assets[name] = b
	rs.digest[name] = "sha256:" + sum(b)
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func resolveAndInstall(t *testing.T, rs *releaseServer, p Package, dir string) error {
	t.Helper()
	p.API = rs.srv.URL + "/api/latest"
	if err := p.Resolve(context.Background(), rs.srv.Client()); err != nil {
		return err
	}
	return Install(context.Background(), rs.srv.Client(), dir, p, nil)
}

func TestInstallSingleFile(t *testing.T) {
	rs := newReleaseServer(t)
	rs.add("yt-dlp.exe", []byte("the real yt-dlp"))
	dir := t.TempDir()
	p := Package{Tool: "yt-dlp", Asset: "yt-dlp.exe", Files: map[string]string{"": "yt-dlp.exe"}}
	p.API = rs.srv.URL + "/api/latest"
	if err := p.Resolve(context.Background(), rs.srv.Client()); err != nil {
		t.Fatal(err)
	}
	if p.Size != int64(len("the real yt-dlp")) || p.SHA256 != sum([]byte("the real yt-dlp")) {
		t.Errorf("resolved %+v", p)
	}
	var last int64
	if err := Install(context.Background(), rs.srv.Client(), dir, p, func(done, total int64) { last = done }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "yt-dlp.exe")); string(b) != "the real yt-dlp" || last != p.Size {
		t.Errorf("installed %q, last progress %d", b, last)
	}
	assertOnly(t, dir, "yt-dlp.exe")
}

// From ffmpeg's archive only the programs Siphon needs are taken, wherever
// the archive's top folder is.
func TestInstallTakesFilesFromZip(t *testing.T) {
	rs := newReleaseServer(t)
	rs.add("ffmpeg.zip", zipOf(t, map[string]string{
		"ffmpeg-n8-win64/bin/ffmpeg.exe":  "ffmpeg",
		"ffmpeg-n8-win64/bin/ffprobe.exe": "ffprobe",
		"ffmpeg-n8-win64/bin/ffplay.exe":  "ffplay",
		"ffmpeg-n8-win64/doc/README.txt":  "docs",
	}))
	dir := t.TempDir()
	p := Package{Tool: "ffmpeg", Asset: "ffmpeg.zip",
		Files: map[string]string{"bin/ffmpeg.exe": "ffmpeg.exe", "bin/ffprobe.exe": "ffprobe.exe"}}
	if err := resolveAndInstall(t, rs, p, dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"ffmpeg.exe": "ffmpeg", "ffprobe.exe": "ffprobe"} {
		if b, _ := os.ReadFile(filepath.Join(dir, name)); string(b) != want {
			t.Errorf("%s = %q", name, b)
		}
	}
	assertOnly(t, dir, "ffmpeg.exe", "ffprobe.exe")
}

// A download that doesn't match the published checksum installs nothing and
// leaves the tool already there alone.
func TestInstallChecksumMismatchInstallsNothing(t *testing.T) {
	rs := newReleaseServer(t)
	rs.assets["yt-dlp.exe"] = []byte("tampered")
	rs.digest["yt-dlp.exe"] = "sha256:" + sum([]byte("the real yt-dlp"))
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "yt-dlp.exe"), []byte("previous version"), 0o755)

	err := resolveAndInstall(t, rs, Package{Tool: "yt-dlp", Asset: "yt-dlp.exe", Files: map[string]string{"": "yt-dlp.exe"}}, dir)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("got %v, want ErrChecksum", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "yt-dlp.exe")); string(b) != "previous version" {
		t.Errorf("the tool already there was touched: %q", b)
	}
	assertOnly(t, dir, "yt-dlp.exe")
}

// An update replaces the tool in place.
func TestInstallReplacesExisting(t *testing.T) {
	rs := newReleaseServer(t)
	rs.add("yt-dlp.exe", []byte("new"))
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "yt-dlp.exe"), []byte("old"), 0o755)
	if err := resolveAndInstall(t, rs, Package{Tool: "yt-dlp", Asset: "yt-dlp.exe", Files: map[string]string{"": "yt-dlp.exe"}}, dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "yt-dlp.exe")); string(b) != "new" {
		t.Errorf("got %q", b)
	}
	assertOnly(t, dir, "yt-dlp.exe")
}

// Codeberg style: no digest, a SHA256SUMS file in the release.
func TestResolveFromSumsFile(t *testing.T) {
	rs := newReleaseServer(t)
	rs.assets["gallery-dl.exe"] = []byte("gallery")
	rs.assets["SHA256SUMS"] = []byte(sum([]byte("other")) + "  gallery-dl.bin\n" + sum([]byte("gallery")) + "  gallery-dl.exe\n")
	p := Package{Tool: "gallery-dl", Asset: "gallery-dl.exe", Sums: "SHA256SUMS", Files: map[string]string{"": "gallery-dl.exe"}}
	dir := t.TempDir()
	if err := resolveAndInstall(t, rs, p, dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "gallery-dl.exe")); string(b) != "gallery" {
		t.Errorf("got %q", b)
	}
}

// Nothing to check against: not installed.
func TestResolveRefusesWithoutChecksum(t *testing.T) {
	rs := newReleaseServer(t)
	rs.assets["tool.exe"] = []byte("x")
	p := Package{Tool: "tool", Asset: "tool.exe", API: rs.srv.URL + "/api/latest"}
	if err := p.Resolve(context.Background(), rs.srv.Client()); err == nil || !strings.Contains(err.Error(), "no checksum") {
		t.Fatalf("got %v", err)
	}
	p = Package{Tool: "tool", Asset: "missing.exe", API: rs.srv.URL + "/api/latest"}
	if err := p.Resolve(context.Background(), rs.srv.Client()); err == nil || !strings.Contains(err.Error(), "has no missing.exe") {
		t.Fatalf("got %v", err)
	}
}

// Every offered package names an asset and what to install; ffmpeg is only
// offered where an official build exists.
func TestPackagesPerSystem(t *testing.T) {
	has := func(ps []Package, tool string) bool {
		for _, p := range ps {
			if p.Tool == tool {
				return true
			}
		}
		return false
	}
	for _, sys := range [][2]string{{"windows", "amd64"}, {"windows", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "arm64"}, {"darwin", "amd64"}} {
		ps := Packages(sys[0], sys[1])
		if !has(ps, "yt-dlp") || !has(ps, "deno") {
			t.Errorf("%v: %v", sys, ps)
		}
		if has(ps, "ffmpeg") != (sys[0] == "windows") {
			t.Errorf("%v: ffmpeg offered = %v", sys, has(ps, "ffmpeg"))
		}
		for _, p := range ps {
			if p.Asset == "" || p.API == "" || len(p.Files) == 0 {
				t.Errorf("%v: incomplete %+v", sys, p)
			}
		}
	}
	if len(Packages("plan9", "386")) != 0 {
		t.Error("packages offered for an unknown system")
	}
}

// What "Install tools" put in Siphon's tools folder is found.
func TestFindUsesToolsDir(t *testing.T) {
	cfg := t.TempDir()
	userConfigDir = func() (string, error) { return cfg, nil }
	t.Cleanup(func() { userConfigDir = os.UserConfigDir })
	t.Setenv("PATH", t.TempDir())
	dir, _ := ToolsDir()
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "deno.exe"), []byte("x"), 0o755)
	if got := Find(nil); got.Deno != filepath.Join(dir, "deno.exe") || got.YtDlp != "" {
		t.Errorf("Find = %+v", got)
	}
}

// assertOnly: dir holds exactly these files (no .download, .new or .old left).
func assertOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Errorf("folder holds %v, want %v", got, names)
	}
}
