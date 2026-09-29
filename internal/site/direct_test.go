package site

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

func newDirectFor(t *testing.T, h http.Handler) (Resolver, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	r := NewDirect(SiteConfig{Name: DirectName, HTTPClient: srv.Client(), Extra: noTools(t)}.WithDefaults())
	return r, srv
}

// noTools points the tool paths at files that don't exist: a real yt-dlp
// on the machine is never asked (a configured path is the only place looked).
func noTools(t *testing.T) map[string]string {
	missing := filepath.Join(t.TempDir(), "missing.exe")
	return map[string]string{"yt_dlp": missing, "gallery_dl": missing, "ffmpeg": missing}
}

func TestDirectMatchesAnyHTTPLinkAsFallback(t *testing.T) {
	r := NewDirect(SiteConfig{Name: DirectName})
	for u, want := range map[string]bool{
		"https://example.com/a/b/setup.zip": true,
		"http://10.0.0.5:8080/x":            true,
		"  https://example.com/x  ":         true,
		"ftp://example.com/x":               false,
		"magnet:?xt=urn:btih:abc":           false,
		"not a link":                        false,
	} {
		if got := r.Match(u); got != want {
			t.Errorf("Match(%q) = %v, want %v", u, got, want)
		}
	}
	if f, ok := r.(Fallback); !ok || !f.Fallback() {
		t.Error("direct must be a fallback: real sites have to be asked first")
	}
}

// The usual case: the name comes from Content-Disposition, the size from the
// range answer, and the item's URL stays the link that was given.
func TestDirectResolvesNameAndSize(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 12345)
	r, srv := newDirectFor(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Range") != "bytes=0-0" {
			t.Errorf("the resolver asked for %q, want the first byte only", req.Header.Get("Range"))
		}
		w.Header().Set("Content-Disposition", `attachment; filename="Holiday video.mp4"`)
		http.ServeContent(w, req, "x", time.Time{}, bytes.NewReader(body))
	}))
	it, err := r.ResolveOne(context.Background(), srv.URL+"/dl?id=7")
	if err != nil {
		t.Fatal(err)
	}
	if it.Filename != "Holiday video.mp4" || it.Size != 12345 {
		t.Errorf("got %q, %d bytes", it.Filename, it.Size)
	}
	if it.URL != srv.URL+"/dl?id=7" || it.SourcePage != it.URL {
		t.Errorf("URL %q / SourcePage %q; both should be the given link", it.URL, it.SourcePage)
	}
	if it.Dir != "" {
		t.Errorf("Dir = %q; a plain link goes to the output root", it.Dir)
	}
}

// A link that redirects to the file ("/download?id=7" -> "/files/setup.zip"):
// the name comes from where it landed; the item keeps the original link, so a
// time-limited redirect target is asked for again on every attempt.
func TestDirectNameAfterRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/download", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/files/setup.zip", http.StatusFound)
	})
	mux.HandleFunc("/files/setup.zip", func(w http.ResponseWriter, req *http.Request) {
		http.ServeContent(w, req, "setup.zip", time.Time{}, bytes.NewReader(make([]byte, 4096)))
	})
	r, srv := newDirectFor(t, mux)
	it, err := r.ResolveOne(context.Background(), srv.URL+"/download?id=7")
	if err != nil {
		t.Fatal(err)
	}
	if it.Filename != "setup.zip" || it.Size != 4096 || it.URL != srv.URL+"/download?id=7" {
		t.Errorf("got %+v", it)
	}
}

// A web page is not a file: refused with a reason, not saved as "the file".
// The same page sent as an attachment is a file.
func TestDirectRefusesWebPages(t *testing.T) {
	attach := false
	r, srv := newDirectFor(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if attach {
			w.Header().Set("Content-Disposition", `attachment; filename="page.html"`)
		}
		fmt.Fprint(w, "<html><body>Please log in</body></html>")
	}))
	_, err := r.ResolveOne(context.Background(), srv.URL+"/login")
	if err == nil || !strings.Contains(err.Error(), "web page") {
		t.Fatalf("a web page was accepted as a file: %v", err)
	}
	if l, _ := LayerOf(err); l != LayerParse {
		t.Errorf("layer %s, want Parse", l)
	}

	attach = true
	it, err := r.ResolveOne(context.Background(), srv.URL+"/save")
	if err != nil || it.Filename != "page.html" {
		t.Errorf("an HTML attachment was refused: %+v %v", it, err)
	}
}

func TestDirectNotFound(t *testing.T) {
	r, srv := newDirectFor(t, http.NotFoundHandler())
	_, err := r.ResolveOne(context.Background(), srv.URL+"/gone.zip")
	if err == nil || !strings.Contains(err.Error(), "doesn't exist") {
		t.Fatalf("404: %v", err)
	}
}

// A server without range support answers 200 with the whole file: the size
// comes from Content-Length. With neither, the size is unknown (-1), not 0.
func TestDirectSizeWithoutRanges(t *testing.T) {
	chunked := false
	r, srv := newDirectFor(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		if !chunked {
			w.Header().Set("Content-Length", "2048")
		}
		w.WriteHeader(http.StatusOK)
		if chunked {
			// Headers out before the body: otherwise the server adds a
			// Content-Length for a small body by itself.
			w.(http.Flusher).Flush()
		}
		_, _ = w.Write(make([]byte, 2048))
	}))
	it, err := r.ResolveOne(context.Background(), srv.URL+"/a.zip")
	if err != nil || it.Size != 2048 {
		t.Fatalf("200 with Content-Length: %+v %v", it, err)
	}
	chunked = true
	it, err = r.ResolveOne(context.Background(), srv.URL+"/b.zip")
	if err != nil || it.Size != -1 {
		t.Fatalf("no length: size %d, want -1 (%v)", it.Size, err)
	}
}

func TestDirectFilenames(t *testing.T) {
	cases := []struct {
		cd, final, orig, ctype, want string
	}{
		// RFC 5987: the UTF-8 name wins over the ASCII fallback.
		{`attachment; filename="a.txt"; filename*=UTF-8''%C3%BCber%20%C3%A7ay.txt`, "", "https://x/y", "", "über çay.txt"},
		{"", "https://x/files/My%20Song.mp3", "https://x/dl", "", "My Song.mp3"},
		{"", "https://x/", "https://x/files/report", "application/pdf", "report.pdf"},
		{"", "https://cdn.example.com/", "https://cdn.example.com/", "", "cdn.example.com"},
	}
	for _, c := range cases {
		_, cdName := disposition(c.cd)
		if got := directFilename(cdName, c.final, c.orig, c.ctype); got != c.want {
			t.Errorf("directFilename(%q, %q) = %q, want %q", c.cd, c.final, got, c.want)
		}
	}
}

// A network failure keeps its type: the queue tells "the network is down"
// (wait and retry) from a real answer by it.
func TestDirectNetworkErrorKeepsItsType(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/x.zip"
	srv.Close()
	r := NewDirect(SiteConfig{Name: DirectName}.WithDefaults())
	_, err := r.ResolveOne(context.Background(), url)
	var op *net.OpError
	if !errors.As(err, &op) {
		t.Fatalf("the network error lost its type: %T %v", err, err)
	}
}

// Hosts handed over by ExcludeHosts (the real sites') are never plain links.
func TestDirectStaysOffRealSitesHosts(t *testing.T) {
	r := NewDirect(SiteConfig{Name: DirectName})
	r.(HostExcluder).ExcludeHosts([]string{"mega.nz", "*.mega.nz", "*.userstorage.mega.co.nz"})
	for u, want := range map[string]bool{
		"https://mega.nz/file/x#bad":                 false,
		"https://www.mega.nz/":                       false,
		"https://gfs1.userstorage.mega.co.nz/dl/abc": false,
		"https://example.com/mega.nz/file.zip":       true, // only the host counts
		"https://notmega.nz/file.zip":                true,
	} {
		if got := r.Match(u); got != want {
			t.Errorf("Match(%q) = %v, want %v", u, got, want)
		}
	}
}

// --- Web pages handed to yt-dlp / gallery-dl ---

// pageServer answers every path with an HTML page (a video or gallery page,
// as far as a plain client can tell), counting the requests.
func pageServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body>a page</body></html>")
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// withTools: the direct resolver with the fake yt-dlp and gallery-dl.
func withTools(t *testing.T, srv *httptest.Server) (Resolver, func() [][]string) {
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
	missing := filepath.Join(t.TempDir(), "missing.exe")
	r := NewDirect(SiteConfig{Name: DirectName, HTTPClient: srv.Client(),
		Extra: map[string]string{"yt_dlp": exe, "gallery_dl": exe, "ffmpeg": missing}}.WithDefaults())
	return r, calls
}

func resolveAll(t *testing.T, r Resolver, u string) ([]Item, error) {
	t.Helper()
	var items []Item
	_, err := r.Resolve(context.Background(), u, func(it Item) error { items = append(items, it); return nil })
	return items, err
}

// A playlist page: one item per video, in a folder named after the playlist,
// each tagged for yt-dlp so it goes back to yt-dlp after a restart.
func TestDirectPageGoesToYtDlp(t *testing.T) {
	srv, _ := pageServer(t)
	r, _ := withTools(t, srv)
	items, err := resolveAll(t, r, srv.URL+"/playlist")
	if err != nil || len(items) != 2 {
		t.Fatalf("got %+v %v", items, err)
	}
	it := items[1]
	if it.SourcePage != ViaYtDlp+srv.URL+"/watch?v=two" || it.URL != srv.URL+"/watch?v=two" ||
		it.Dir != "My list" || it.Filename != "Video two [two]" || it.Index != 1 {
		t.Errorf("item: %+v", it)
	}
	if !r.(SelfDownloader).Handles(it) {
		t.Error("a yt-dlp item isn't downloaded by the tool")
	}
}

// A page yt-dlp doesn't know goes to gallery-dl, as one item for the gallery.
func TestDirectPageGoesToGalleryDL(t *testing.T) {
	srv, _ := pageServer(t)
	r, calls := withTools(t, srv)
	items, err := resolveAll(t, r, srv.URL+"/album")
	if err != nil || len(items) != 1 {
		t.Fatalf("got %+v %v", items, err)
	}
	if it := items[0]; it.SourcePage != ViaGalleryDL+srv.URL+"/album" || it.Filename != "127.0.0.1 album" || it.Size != -1 {
		t.Errorf("item: %+v", it)
	}
	if n := len(calls()); n != 2 {
		t.Errorf("%d tool calls, want 2 (yt-dlp said no, gallery-dl said yes)", n)
	}
}

// yt-dlp knows the site but can't get the video, and gallery-dl can't take it
// either: yt-dlp's reason is the answer (more telling than "unsupported").
func TestDirectKeepsYtDlpReason(t *testing.T) {
	srv, _ := pageServer(t)
	r, calls := withTools(t, srv)
	_, err := resolveAll(t, r, srv.URL+"/private")
	if err == nil || !strings.Contains(err.Error(), "Private video") {
		t.Fatalf("got %v", err)
	}
	if n := len(calls()); n != 2 {
		t.Errorf("%d tool calls, want 2 (yt-dlp failed, gallery-dl said no)", n)
	}
}

func TestDirectPageNobodyKnows(t *testing.T) {
	srv, _ := pageServer(t)
	r, _ := withTools(t, srv)
	_, err := resolveAll(t, r, srv.URL+"/about")
	if err == nil || !strings.Contains(err.Error(), "neither Siphon nor yt-dlp nor gallery-dl") {
		t.Fatalf("got %v", err)
	}
}

// Without the tools, a page is refused with a pointer to them.
func TestDirectPageWithoutToolsSuggestsThem(t *testing.T) {
	srv, _ := pageServer(t)
	r := NewDirect(SiteConfig{Name: DirectName, HTTPClient: srv.Client(), Extra: noTools(t)}.WithDefaults())
	_, err := resolveAll(t, r, srv.URL+"/watch?v=one")
	if err == nil || !strings.Contains(err.Error(), "install yt-dlp or gallery-dl") {
		t.Fatalf("got %v", err)
	}
}

// A tool prefix sends the link straight to that tool: no probe of the page,
// even on a real site's host (the user's explicit choice).
func TestDirectToolPrefix(t *testing.T) {
	srv, hits := pageServer(t)
	r, _ := withTools(t, srv)
	r.(HostExcluder).ExcludeHosts([]string{"127.0.0.1"})
	if r.Match(srv.URL + "/album") {
		t.Error("an excluded host was matched without a prefix")
	}
	if !r.Match(ViaGalleryDL + srv.URL + "/album") {
		t.Error("an explicit tool prefix wasn't matched")
	}
	items, err := resolveAll(t, r, ViaGalleryDL+srv.URL+"/album")
	if err != nil || len(items) != 1 || items[0].SourcePage != ViaGalleryDL+srv.URL+"/album" {
		t.Fatalf("got %+v %v", items, err)
	}
	if hits.Load() != 0 {
		t.Errorf("the page was probed %d times; a tool prefix skips that", hits.Load())
	}
	// Forced to yt-dlp, an album is not tried with gallery-dl.
	if _, err := resolveAll(t, r, ViaYtDlp+srv.URL+"/album"); err == nil {
		t.Error("yt-dlp: prefix fell back to gallery-dl")
	}
}

// After a restart a tool job resolves again without any request, and the
// tool downloads it.
func TestDirectToolItemDownloads(t *testing.T) {
	srv, hits := pageServer(t)
	r, _ := withTools(t, srv)
	it, err := r.ResolveOne(context.Background(), ViaYtDlp+srv.URL+"/watch?v=one")
	if err != nil || it.URL != srv.URL+"/watch?v=one" || hits.Load() != 0 {
		t.Fatalf("ResolveOne: %+v %v (%d requests)", it, err, hits.Load())
	}
	dir := t.TempDir()
	path, size, err := r.(SelfDownloader).DownloadSelf(context.Background(), dir, it, nil)
	if err != nil || filepath.Dir(path) != dir || size == 0 {
		t.Fatalf("DownloadSelf: %s %d %v", path, size, err)
	}
}

// A plain file link never involves the tools.
func TestDirectFileSkipsTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x.zip", time.Time{}, bytes.NewReader(make([]byte, 100)))
	}))
	t.Cleanup(srv.Close)
	r, calls := withTools(t, srv)
	items, err := resolveAll(t, r, srv.URL+"/x.zip")
	if err != nil || len(items) != 1 || r.(SelfDownloader).Handles(items[0]) {
		t.Fatalf("got %+v %v", items, err)
	}
	if n := len(calls()); n != 0 {
		t.Errorf("the tools were asked %d times about a plain file", n)
	}
}

// MEASURED on imgur: yt-dlp claims an image page and fails ("not a video");
// gallery-dl must still get the chance to take it.
func TestDirectImagePageFallsToGalleryDL(t *testing.T) {
	srv, _ := pageServer(t)
	r, _ := withTools(t, srv)
	items, err := resolveAll(t, r, srv.URL+"/image")
	if err != nil || len(items) != 1 || items[0].SourcePage != ViaGalleryDL+srv.URL+"/image" {
		t.Fatalf("got %+v %v", items, err)
	}
}
