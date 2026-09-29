package site

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Against the real sites; skipped unless SIPHON_LIVE=1:
//
//	SIPHON_LIVE=1 go test ./internal/site -run Live -v
//
// The links are public, neutral files: sample output of an open-source
// e-book tool and a public research dataset, both also used by other
// downloaders' test suites. Nothing is downloaded beyond the checks.
func liveCfg(t *testing.T, cfg SiteConfig) SiteConfig {
	t.Helper()
	if os.Getenv("SIPHON_LIVE") != "1" {
		t.Skip("set SIPHON_LIVE=1 to run against the real sites")
	}
	cfg.UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36"
	cfg.HTTPClient = &http.Client{Timeout: time.Minute}
	cfg.Logf = t.Logf
	return cfg
}

func TestLiveMediafire(t *testing.T) {
	cfg := liveCfg(t, mfCfg())
	cfg.CanaryURLs = []string{"https://www.mediafire.com/file/5iv342h2u39e0t6/kobo_clara.kepub.epub/file"}
	m := NewMediafire(cfg).(*mediafire)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// A folder with folders in it: 2 files at the top, 16 below.
	var items []Item
	itemErrs, err := m.Resolve(ctx, "https://www.mediafire.com/folder/m3sij67rizpb4/XJTU-SY_Bearing_Datasets", func(it Item) error {
		items = append(items, it)
		return nil
	})
	if err != nil || len(itemErrs) > 0 {
		t.Fatalf("folder: %v %v", err, itemErrs)
	}
	if len(items) != 18 {
		t.Errorf("%d files, want 18", len(items))
	}
	nested := 0
	for _, it := range items {
		if !isSHA256(it.SHA256) || it.Size <= 0 {
			t.Errorf("%s: sha256 %q, size %d", it.Filename, it.SHA256, it.Size)
		}
		if strings.HasPrefix(it.Dir, "XJTU-SY_Bearing_Datasets/") {
			nested++
		}
	}
	if nested != 16 {
		t.Errorf("%d files in subfolders, want 16", nested)
	}

	// A single file, then its download link.
	var file Item
	if _, err := m.Resolve(ctx, "https://www.mediafire.com/file/5iv342h2u39e0t6/kobo_clara.kepub.epub/file", func(it Item) error {
		file = it
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if file.Filename != "kobo_clara.kepub.epub" || file.Size != 2345792 ||
		file.SHA256 != "6fca15cb96c9907abecc3453d359ac9a122c70efd7a56d6c99ce42d9c29dfd31" {
		t.Errorf("file = %+v", file)
	}
	link, err := m.PrepareURL(ctx, file.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !MatchHost(hostOf(link), cfg.CDNPatterns) {
		t.Errorf("download link %s is not on a download server", link)
	}

	res, err := m.Diagnose(ctx)
	if err != nil || hasFail(res) {
		t.Errorf("diagnose: %v %+v", err, res)
	}
}

func hostOf(raw string) string {
	u, _, err := parseLink(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// gofile removes files nobody downloads for a while; if this link is gone,
// pick another public one. Only the first 64 KB of the file is fetched.
func TestLiveGofile(t *testing.T) {
	cfg := liveCfg(t, gofileCfg())
	cfg.CanaryURLs = []string{"https://gofile.io/d/PSHbQR"}
	g := NewGofile(cfg).(*gofile)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	items, itemErrs, err := collect(t, g, "https://gofile.io/d/PSHbQR")
	if err != nil || len(itemErrs) > 0 || len(items) != 1 {
		t.Fatalf("resolve: %d items, %v %v", len(items), itemErrs, err)
	}
	it := items[0]
	if it.Filename != "WinDeckOS 2.0.mrimg" || it.Size != 31767283852 || it.Dir != "" {
		t.Errorf("item = %+v", it)
	}

	// The download server takes the session's token as a cookie.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, it.URL, nil)
	req.Header.Set("User-Agent", cfg.UserAgent)
	for k, v := range it.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Range", "bytes=0-65535")
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || isPage(resp) || resp.Header.Get("Last-Modified") == "" {
		t.Errorf("download: %s, %s, Last-Modified %q", resp.Status, resp.Header.Get("Content-Type"), resp.Header.Get("Last-Modified"))
	}

	one, err := g.ResolveOne(ctx, it.SourcePage)
	if err != nil || one.URL != it.URL {
		t.Errorf("ResolveOne = %+v, %v", one, err)
	}
	res, err := g.Diagnose(ctx)
	if err != nil || hasFail(res) {
		t.Errorf("diagnose: %v %+v", err, res)
	}
}

// gallery-dl's own test album: its files were removed, the album page stays.
// It checks the page layout and that the file API answers; no file is
// downloaded.
func TestLiveCyberdrop(t *testing.T) {
	cfg := liveCfg(t, cyberdropCfg())
	cfg.CanaryURLs = []string{"https://cyberdrop.cr/a/8uE0wQiK"}
	c := NewCyberdrop(cfg).(*cyberdrop)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	_, body, err := c.w.get(ctx, "https://cyberdrop.cr/a/8uE0wQiK?nojs", nil)
	if err != nil {
		t.Fatal(err)
	}
	title, count, _ := cdAlbumPage(string(body))
	if title != `test テスト "&>` || count < 0 {
		t.Errorf("album page: title %q, count %d; did the layout change?", title, count)
	}
	// An old domain resolves through the current one.
	if _, _, err := collect(t, c, "https://cyberdrop.me/a/8uE0wQiK"); err == nil || !strings.Contains(err.Error(), "no files") {
		t.Errorf("empty album: %v", err)
	}
	var cdErr *CyberdropError
	if _, _, err := collect(t, c, "https://cyberdrop.cr/f/ntY9Vvkh73rUB"); !errors.As(err, &cdErr) || cdErr.Status != http.StatusNotFound {
		t.Errorf("removed file: %v, want the API's 404", err)
	}
	res, err := c.Diagnose(ctx)
	if err != nil || hasFail(res) {
		t.Errorf("diagnose: %v %+v", err, res)
	}
}
