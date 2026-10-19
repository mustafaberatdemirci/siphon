package run

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// End to end: the real cyberfile resolver and the real downloader against a
// fake cyberfile answering in the shapes measured on 2026-10-19 (see
// site/cyberfile.go). Each attempt gets a fresh download link; a stale one
// ends on the site's error page and is replaced; a file whose storage lost
// its data fails once, with the reason, instead of being retried.

type cfE2E struct {
	srv    *httptest.Server
	client *http.Client
	body   []byte

	mu        sync.Mutex
	seq       int
	tokens    map[string]bool
	reject    int  // the next this many valid tokens are refused as stale
	missing   bool // valid tokens get 404 naming the file
	downloads int  // requests to the download server
}

func newCfE2E(t *testing.T) *cfE2E {
	t.Helper()
	e := &cfE2E{body: make([]byte, 300<<10), tokens: map[string]bool{}}
	_, _ = rand.Read(e.body)
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		switch host := r.Header.Get("X-Host"); {
		case host == "p2.cyberfile.me":
			e.downloads++
			tok := r.URL.Query().Get("download_token")
			if !e.tokens[tok] || e.reject > 0 {
				if e.reject > 0 {
					e.reject--
				}
				http.Redirect(w, r, "https://cyberfile.me/error?e=File+can+not+be+located", http.StatusFound)
				return
			}
			w.Header().Set("Content-Disposition", `attachment; filename="clip.mp4"`)
			if e.missing {
				w.Header().Set("Content-Type", "text/html; charset=UTF-8")
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, "<html>404 Oops - Page Not Found!</html>")
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(e.body))
		case r.URL.Path == "/error":
			fmt.Fprint(w, "<html>File can not be located, please try again later.</html>")
		case r.Method == http.MethodPost && r.URL.Path == "/account/ajax/file_details":
			e.seq++
			tok := fmt.Sprintf("%064d", e.seq)
			e.tokens[tok] = true
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "page_title": "clip.mp4",
				"html": `<button onClick="openUrl('https://p2.cyberfile.me/AbCd/clip.mp4?download_token=` + tok + `'); return false;">Download</button>`})
		case r.URL.Path == "/AbCd":
			fmt.Fprint(w, `<script>showFileInformation(4242);</script>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(e.srv.Close)
	to, _ := url.Parse(e.srv.URL)
	e.client = &http.Client{Transport: hostRewrite{to: to}}
	return e
}

func (e *cfE2E) worker(t *testing.T) (site.Resolver, *Worker) {
	cfg := site.SiteConfig{
		Name:        site.CyberfileName,
		Domains:     []string{"cyberfile.me", "www.cyberfile.me"},
		CDNPatterns: []string{"*.cyberfile.me"},
		MaxSegments: 1, MaxConcurrent: 3, MaxRetries: 3,
		BaseDelay: time.Millisecond, MaxDelay: time.Millisecond,
		HTTPClient: e.client,
	}.WithDefaults()
	r := site.NewCyberfile(cfg)
	return r, NewWorker(r, cfg, e.client, Events{})
}

func (e *cfE2E) item(t *testing.T, r site.Resolver) site.Item {
	t.Helper()
	var items []site.Item
	if _, err := r.Resolve(context.Background(), "https://cyberfile.me/AbCd", func(it site.Item) error {
		items = append(items, it)
		return nil
	}); err != nil || len(items) != 1 {
		t.Fatalf("Resolve: %d items, %v", len(items), err)
	}
	return items[0]
}

func TestCyberfileEndToEnd(t *testing.T) {
	e := newCfE2E(t)
	r, w := e.worker(t)
	out := tempDir(t)
	o := w.DownloadItem(context.Background(), out, nil, e.item(t, r), Events{})
	if o.Kind != OutcomeDone {
		t.Fatalf("outcome %v: %v", o.Kind, o.Err)
	}
	if got, _ := os.ReadFile(o.Result.Path); !bytes.Equal(got, e.body) {
		t.Fatal("content is corrupt")
	}
}

// A link that went stale before its turn ends on the site's error page: the
// file is resolved again and the download goes on with a fresh link.
func TestCyberfileEndToEndStaleLink(t *testing.T) {
	e := newCfE2E(t)
	e.reject = 1
	r, w := e.worker(t)
	o := w.DownloadItem(context.Background(), tempDir(t), nil, e.item(t, r), Events{})
	if o.Kind != OutcomeDone {
		t.Fatalf("outcome %v: %v", o.Kind, o.Err)
	}
	if e.downloads != 2 {
		t.Errorf("%d download requests, want 2 (the stale link, then a fresh one)", e.downloads)
	}
}

// The case measured on the live site: every valid link gets a 404 naming the
// file. The download fails with the reason, without retrying it.
func TestCyberfileEndToEndMissingStorage(t *testing.T) {
	e := newCfE2E(t)
	e.missing = true
	r, w := e.worker(t)
	o := w.DownloadItem(context.Background(), tempDir(t), nil, e.item(t, r), Events{})
	if o.Kind != OutcomeFailed || o.Err == nil || !strings.Contains(o.Err.Error(), "storage server can't find the file") {
		t.Fatalf("outcome %v: %v", o.Kind, o.Err)
	}
	if e.downloads != 1 {
		t.Errorf("%d download requests, want 1: a missing file isn't retried", e.downloads)
	}
}
