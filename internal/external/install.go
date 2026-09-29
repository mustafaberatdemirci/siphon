package external

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Installing the tools is the user's choice, never automatic: "Install
// tools" in the Diagnose tab or "siphon tools install". Every file comes from
// the tool's own official release, is checked against the SHA-256 published
// with that release, and goes into ToolsDir. Nothing is installed if a check
// fails.
//
// MEASURED (2026-09-29): GitHub's release API gives every asset a "digest"
// (sha256:…) and a size; gallery-dl moved to Codeberg, whose API has the same
// shape but no digest, and publishes a SHA256SUMS file with each release.

// Package is one tool that can be installed on this system.
type Package struct {
	Tool    string // "yt-dlp", "ffmpeg", "gallery-dl", "deno"
	Purpose string // one line for the dialog
	API     string // the release API URL (GitHub or Codeberg, same shape)
	Asset   string // the file to download from that release
	// Files maps the file to take (for an archive, a path suffix inside it;
	// otherwise the asset itself, key "") to the name installed in ToolsDir.
	Files map[string]string
	// Sums is the checksum file of the release, for hosts without a digest.
	Sums string

	// Filled by Resolve.
	URL    string
	Size   int64
	SHA256 string
}

// Packages lists what can be installed on goos/goarch. Where a tool has no
// official build to take (ffmpeg on Linux and macOS, gallery-dl on macOS),
// it isn't offered: the system's package manager is the better source there.
func Packages(goos, goarch string) []Package {
	const (
		ytAPI      = "https://api.github.com/repos/yt-dlp/yt-dlp/releases/latest"
		ffAPI      = "https://api.github.com/repos/yt-dlp/FFmpeg-Builds/releases/latest"
		denoAPI    = "https://api.github.com/repos/denoland/deno/releases/latest"
		galleryAPI = "https://codeberg.org/api/v1/repos/mikf/gallery-dl/releases/latest"
	)
	exe := ""
	if goos == "windows" {
		exe = ".exe"
	}
	yt := Package{Tool: "yt-dlp", Purpose: "video and audio from about 1,800 sites", API: ytAPI}
	ff := Package{Tool: "ffmpeg", Purpose: "merges picture and sound that sites send separately", API: ffAPI,
		Files: map[string]string{"bin/ffmpeg.exe": "ffmpeg.exe", "bin/ffprobe.exe": "ffprobe.exe"}}
	deno := Package{Tool: "deno", Purpose: "JavaScript runtime yt-dlp wants for YouTube", API: denoAPI,
		Files: map[string]string{"deno" + exe: "deno" + exe}}
	gallery := Package{Tool: "gallery-dl", Purpose: "images and galleries from about 300 sites", API: galleryAPI,
		Sums: "SHA256SUMS", Files: map[string]string{"": "gallery-dl" + exe}}

	var out []Package
	switch goos + "/" + goarch {
	case "windows/amd64":
		yt.Asset = "yt-dlp.exe"
		ff.Asset = "ffmpeg-master-latest-win64-gpl.zip"
		deno.Asset = "deno-x86_64-pc-windows-msvc.zip"
		gallery.Asset = "gallery-dl.exe"
		out = []Package{yt, ff, gallery, deno}
	case "windows/arm64":
		yt.Asset = "yt-dlp_arm64.exe"
		ff.Asset = "ffmpeg-master-latest-winarm64-gpl.zip"
		deno.Asset = "deno-aarch64-pc-windows-msvc.zip"
		gallery.Asset = "gallery-dl.exe" // x64, runs under emulation
		out = []Package{yt, ff, gallery, deno}
	case "linux/amd64":
		yt.Asset = "yt-dlp_linux"
		deno.Asset = "deno-x86_64-unknown-linux-gnu.zip"
		gallery.Asset = "gallery-dl.bin"
		out = []Package{yt, gallery, deno}
	case "linux/arm64":
		yt.Asset = "yt-dlp_linux_aarch64"
		deno.Asset = "deno-aarch64-unknown-linux-gnu.zip"
		out = []Package{yt, deno}
	case "darwin/amd64", "darwin/arm64":
		yt.Asset = "yt-dlp_macos"
		deno.Asset = "deno-x86_64-apple-darwin.zip"
		if goarch == "arm64" {
			deno.Asset = "deno-aarch64-apple-darwin.zip"
		}
		out = []Package{yt, deno}
	}
	for i := range out {
		if out[i].Files == nil {
			out[i].Files = map[string]string{"": out[i].Tool + exe}
		}
	}
	return out
}

type release struct {
	Tag    string `json:"tag_name"`
	Assets []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		URL    string `json:"browser_download_url"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

// Resolve asks the release API for the asset's URL, size and SHA-256.
func (p *Package) Resolve(ctx context.Context, client *http.Client) error {
	var rel release
	if err := getJSON(ctx, client, p.API, &rel); err != nil {
		return fmt.Errorf("%s: release information: %w", p.Tool, err)
	}
	var sumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case p.Asset:
			p.URL, p.Size = a.URL, a.Size
			if hexSum, ok := strings.CutPrefix(a.Digest, "sha256:"); ok && isSHA256(hexSum) {
				p.SHA256 = strings.ToLower(hexSum)
			}
		case p.Sums:
			sumsURL = a.URL
		}
	}
	if p.URL == "" {
		return fmt.Errorf("%s: the latest release (%s) has no %s", p.Tool, rel.Tag, p.Asset)
	}
	if p.SHA256 == "" && sumsURL != "" {
		sum, err := sumFromFile(ctx, client, sumsURL, p.Asset)
		if err != nil {
			return fmt.Errorf("%s: %w", p.Tool, err)
		}
		p.SHA256 = sum
	}
	if p.SHA256 == "" {
		// Never install something that can't be checked.
		return fmt.Errorf("%s: the release publishes no checksum for %s; not installing it", p.Tool, p.Asset)
	}
	return nil
}

func getJSON(ctx context.Context, client *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "siphon") // GitHub's API refuses requests without one
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v)
}

// sumFromFile reads a "sha256sum" style file ("<hex>  <name>" or
// "<hex> *<name>") and returns the line for asset.
func sumFromFile(ctx context.Context, client *http.Client, url, asset string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("checksum file: HTTP %s", resp.Status)
	}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset && isSHA256(f[0]) {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("the checksum file doesn't list %s", asset)
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ErrChecksum says a download didn't match the published SHA-256.
var ErrChecksum = errors.New("checksum mismatch")

// Install downloads a resolved package into dir, checks it, and puts its
// files in place. progress may be nil. A tool already there is replaced even
// while it runs (Windows can't overwrite a running .exe, but it can rename
// it out of the way).
func Install(ctx context.Context, client *http.Client, dir string, p Package, progress func(done, total int64)) error {
	if p.URL == "" || p.SHA256 == "" {
		return fmt.Errorf("%s: not resolved", p.Tool)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, p.Asset+".download")
	defer os.Remove(tmp)
	if err := download(ctx, client, p, tmp, progress); err != nil {
		return err
	}

	// Stage every file first; move them in only once all are there.
	staged := map[string]string{} // staged path -> installed name
	defer func() {
		for s := range staged {
			_ = os.Remove(s)
		}
	}()
	if src, whole := p.Files[""]; whole && len(p.Files) == 1 {
		staged[tmp] = src
	} else {
		if err := extract(tmp, dir, p.Files, staged); err != nil {
			return fmt.Errorf("%s: %w", p.Tool, err)
		}
	}
	for s, name := range staged {
		if err := replace(s, filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("%s: %w", p.Tool, err)
		}
		delete(staged, s)
	}
	return nil
}

func download(ctx context.Context, client *http.Client, p Package, to string, progress func(done, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "siphon")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", p.Tool, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: download: HTTP %s", p.Tool, resp.Status)
	}
	f, err := os.Create(to)
	if err != nil {
		return err
	}
	h := sha256.New()
	var done int64
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return werr
			}
			h.Write(buf[:n])
			done += int64(n)
			if progress != nil {
				progress(done, p.Size)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return fmt.Errorf("%s: %w", p.Tool, rerr)
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != p.SHA256 {
		return fmt.Errorf("%s: %w: expected %s, got %s — nothing installed", p.Tool, ErrChecksum, p.SHA256, got)
	}
	return nil
}

// extract takes the wanted files out of a zip: each key of files is a path
// suffix inside the archive (ffmpeg's zip has a versioned top folder).
func extract(archive, dir string, files map[string]string, staged map[string]string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for suffix, name := range files {
		var found *zip.File
		for _, f := range zr.File {
			n := path.Clean(strings.ReplaceAll(f.Name, `\`, "/"))
			if n == suffix || strings.HasSuffix(n, "/"+suffix) {
				found = f
				break
			}
		}
		if found == nil {
			return fmt.Errorf("the archive has no %s", suffix)
		}
		out := filepath.Join(dir, name+".new")
		if err := extractOne(found, out); err != nil {
			return err
		}
		staged[out] = name
	}
	return nil
}

func extractOne(f *zip.File, to string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	w, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, rc); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

// replace moves src to dst. A dst that exists (maybe running) is renamed out
// of the way first and removed if possible; a leftover is cleaned next time.
func replace(src, dst string) error {
	old := dst + ".old"
	_ = os.Remove(old)
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, old); err != nil {
			return err
		}
	}
	if err := os.Rename(src, dst); err != nil {
		_ = os.Rename(old, dst) // put the previous one back
		return err
	}
	_ = os.Chmod(dst, 0o755)
	_ = os.Remove(old)
	return nil
}
