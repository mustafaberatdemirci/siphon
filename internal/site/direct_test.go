package site

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newDirectFor(t *testing.T, h http.Handler) (Resolver, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	r := NewDirect(SiteConfig{Name: DirectName, HTTPClient: srv.Client()}.WithDefaults())
	return r, srv
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
