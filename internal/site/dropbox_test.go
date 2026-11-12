package site

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func dropboxCfg() SiteConfig {
	return SiteConfig{
		Name:        DropboxName,
		Domains:     []string{"www.dropbox.com", "dropbox.com"},
		CDNPatterns: []string{"*.dl.dropboxusercontent.com", "dl.dropboxusercontent.com"},
		CanaryURLs:  []string{"https://www.dropbox.com/s/nelirfsxnmcfbfh/test.mp4?dl=0"},
	}.WithDefaults()
}

func TestDropboxParse(t *testing.T) {
	d := NewDropbox(dropboxCfg()).(*dropbox)
	for raw, want := range map[string]dbLink{
		"https://www.dropbox.com/scl/fi/cttkzvl75/video.mp4?rlkey=zae0&dl=0": {
			download: "https://www.dropbox.com/scl/fi/cttkzvl75/video.mp4?dl=1&rlkey=zae0",
			page:     "https://www.dropbox.com/scl/fi/cttkzvl75/video.mp4?rlkey=zae0"},
		"dropbox.com/s/nelirfsx/test.mp4?raw=1": {
			download: "https://www.dropbox.com/s/nelirfsx/test.mp4?dl=1",
			page:     "https://www.dropbox.com/s/nelirfsx/test.mp4"},
		"https://www.dropbox.com/scl/fo/ekhsv/AB064?rlkey=jw3w&dl=0": {folder: true,
			download: "https://www.dropbox.com/scl/fo/ekhsv/AB064?dl=1&rlkey=jw3w",
			page:     "https://www.dropbox.com/scl/fo/ekhsv/AB064?rlkey=jw3w"},
		"https://www.dropbox.com/sh/2mgpiuq/AABy-fW4?dl=0&preview=a.mp4": {folder: true,
			download: "https://www.dropbox.com/sh/2mgpiuq/AABy-fW4?dl=1",
			page:     "https://www.dropbox.com/sh/2mgpiuq/AABy-fW4"},
		"https://uc6f47.dl.dropboxusercontent.com/cd/0/inline/DLx/file": {
			download: "https://uc6f47.dl.dropboxusercontent.com/cd/0/inline/DLx/file",
			page:     "https://uc6f47.dl.dropboxusercontent.com/cd/0/inline/DLx/file"},
	} {
		got, err := d.parse(raw)
		if err != nil || got != want {
			t.Errorf("parse(%q) = %+v, %v\nwant %+v", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"https://www.dropbox.com/",
		"https://www.dropbox.com/home/Photos",
		"https://www.dropbox.com/s/onlykey",
		"https://example.com/s/abc/file.zip",
	} {
		if d.Match(raw) {
			t.Errorf("%q was taken", raw)
		}
	}
}

// A shared link with dl=1 redirects to a download server that names the
// file; a removed one answers 200 with an error page.
func TestDropboxResolve(t *testing.T) {
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{
		"www.dropbox.com": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/s/gone/") {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte("<title>Dropbox - Error - Simplify your life</title>"))
				return
			}
			if r.URL.Query().Get("dl") != "1" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte("<title>preview</title>"))
				return
			}
			http.Redirect(w, r, "https://uc1.dl.dropboxusercontent.com/cd/0/inline/X/file?dl=1", http.StatusFound)
		},
		"uc1.dl.dropboxusercontent.com": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/binary")
			w.Header().Set("Content-Disposition", `attachment; filename="video.mp4"; filename*=UTF-8''v%C3%ADdeo.mp4`)
			w.Header().Set("Content-Range", "bytes 0-0/1601434")
			w.Header().Set("Etag", "355d")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("x"))
		},
	}}
	cfg := dropboxCfg()
	cfg.HTTPClient = &http.Client{Transport: rt}
	d := NewDropbox(cfg)
	items, _, err := collect(t, d, "https://www.dropbox.com/s/abc/video.mp4?dl=0")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Filename != "vídeo.mp4" || items[0].Size != 1601434 ||
		items[0].URL != "https://www.dropbox.com/s/abc/video.mp4?dl=1" || items[0].SourcePage != "https://www.dropbox.com/s/abc/video.mp4" {
		t.Errorf("file: %+v", items)
	}
	if _, _, err := collect(t, d, "https://www.dropbox.com/s/gone/x.zip"); err == nil || !strings.Contains(err.Error(), "answered with a page") {
		t.Errorf("removed link: %v", err)
	}
}

// Against the real services (SIPHON_LIVE=1): gdown's test folder and
// yt-dlp's test video, both used by those projects' own tests. Only first
// bytes are asked for.
func TestLiveGdrive(t *testing.T) {
	cfg := liveCfg(t, gdriveCfg())
	g := NewGdrive(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	items, itemErrs, err := collect(t, g, "https://drive.google.com/drive/folders/1KpLl_1tcK0eeehzN980zbG-3M2nhbVks")
	if err != nil || len(itemErrs) > 0 {
		t.Fatalf("folder: %v %v", err, itemErrs)
	}
	nested := 0
	for _, it := range items {
		if strings.HasPrefix(it.Dir, "gdown_folder_test/") {
			nested++
		}
	}
	if len(items) != 6 || nested != 3 {
		t.Errorf("%d files, %d in subfolders; want 6 and 3", len(items), nested)
	}
	// A large file comes without the virus-scan page.
	big, _, err := collect(t, g, "https://drive.google.com/file/d/1l_5RK28JRL19wpT22B-DY9We3TVXnnQQ/view")
	if err != nil || len(big) != 1 || big[0].Filename != "fcn8s_from_caffe.npz" || big[0].Size != 498881336 {
		t.Errorf("large file: %+v %v", big, err)
	}
	res, err := g.Diagnose(ctx)
	if err != nil || hasFail(res) {
		t.Errorf("diagnose: %v %+v", err, res)
	}
}

func TestLiveDropbox(t *testing.T) {
	cfg := liveCfg(t, dropboxCfg())
	cfg.CanaryURLs = []string{"https://www.dropbox.com/s/nelirfsxnmcfbfh/youtube-dl%20test%20video%20%27%C3%A4%22BaW_jenozKc.mp4?dl=0"}
	d := NewDropbox(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	items, _, err := collect(t, d, cfg.CanaryURLs[0])
	if err != nil || len(items) != 1 || items[0].Filename != "youtube-dl test video 'äBaW_jenozKc.mp4" || items[0].Size != 1601434 {
		t.Errorf("file: %+v %v", items, err)
	}
	res, err := d.Diagnose(ctx)
	if err != nil || hasFail(res) {
		t.Errorf("diagnose: %v %+v", err, res)
	}
}
