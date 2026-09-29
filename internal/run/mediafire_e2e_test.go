package run

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// End to end: the real mediafire resolver and the real downloader against a
// fake mediafire whose download server, like the real one (measured
// 2026-09-29), takes Range requests but sends neither ETag nor
// Last-Modified. The SHA-256 from the API has to be what lets the file use
// several connections, and the folder inside the folder has to become a
// folder on disk.

// hostRewrite sends every request to one test server, keeping the host it
// was meant for in X-Host.
type hostRewrite struct{ to *url.URL }

func (h hostRewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("X-Host", req.URL.Host)
	r.URL.Scheme, r.URL.Host, r.Host = h.to.Scheme, h.to.Host, ""
	return http.DefaultTransport.RoundTrip(r)
}

type mfE2E struct {
	srv    *httptest.Server
	client *http.Client
	body   []byte

	mu       sync.Mutex
	pages    int // file page fetches (one per attempt)
	ranges   int // download requests with a Range header
	ifRanges int // download requests with If-Range
}

func newMfE2E(t *testing.T, size int) *mfE2E {
	t.Helper()
	e := &mfE2E{body: make([]byte, size)}
	_, _ = rand.Read(e.body)
	sum := sha256.Sum256(e.body)
	file := map[string]any{"quickkey": "quickkey001", "filename": "data.bin", "size": fmt.Sprint(size),
		"hash": hex.EncodeToString(sum[:]), "password_protected": "no"}
	api := func(w http.ResponseWriter, resp map[string]any) {
		resp["result"] = "Success"
		_ = json.NewEncoder(w).Encode(map[string]any{"response": resp})
	}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Header.Get("X-Host") == "download7.mediafire.com":
			e.mu.Lock()
			if r.Header.Get("Range") != "" {
				e.ranges++
			}
			if r.Header.Get("If-Range") != "" {
				e.ifRanges++
			}
			e.mu.Unlock()
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(e.body))
		case r.URL.Path == "/api/1.4/folder/get_info.php":
			api(w, map[string]any{"folder_info": map[string]any{"folderkey": q.Get("folder_key"), "name": "Outer",
				"file_count": "0", "folder_count": "1"}})
		case r.URL.Path == "/api/1.4/folder/get_content.php" && q.Get("folder_key") == "outerfolder":
			api(w, map[string]any{"folder_content": map[string]any{"more_chunks": "no", "files": []any{},
				"folders": []any{map[string]any{"folderkey": "innerfolder", "name": "Inner", "file_count": "1", "folder_count": "0"}}}})
		case r.URL.Path == "/api/1.4/folder/get_content.php" && q.Get("folder_key") == "innerfolder":
			api(w, map[string]any{"folder_content": map[string]any{"more_chunks": "no", "files": []any{file}}})
		case r.URL.Path == "/api/1.4/file/get_info.php":
			api(w, map[string]any{"file_info": file})
		case r.URL.Path == "/file/quickkey001":
			e.mu.Lock()
			e.pages++
			e.mu.Unlock()
			fmt.Fprint(w, `<a href="https://download7.mediafire.com/dkey/quickkey001/data.bin" id="downloadButton">Download</a>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(e.srv.Close)
	to, _ := url.Parse(e.srv.URL)
	e.client = &http.Client{Transport: hostRewrite{to: to}}
	return e
}

func (e *mfE2E) cfg() site.SiteConfig {
	return site.SiteConfig{
		Name:        site.MediafireName,
		Domains:     []string{"www.mediafire.com", "mediafire.com"},
		CDNPatterns: []string{"download*.mediafire.com"},
		MaxSegments: 4, MaxConcurrent: 4, MaxRetries: 2,
		HTTPClient: e.client,
	}.WithDefaults()
}

func TestMediafireEndToEndSegmentedWithoutValidator(t *testing.T) {
	e := newMfE2E(t, 1<<20+77)
	cfg := e.cfg()
	r := site.NewMediafire(cfg)
	w := NewWorker(r, cfg, e.client, Events{})
	w.Down.MinSegmentSize = 64 << 10

	var items []site.Item
	if _, err := r.Resolve(context.Background(), "https://www.mediafire.com/folder/outerfolder/Outer", func(it site.Item) error {
		items = append(items, it)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("%d items, want 1", len(items))
	}

	out := tempDir(t)
	outc := w.DownloadItem(context.Background(), out, nil, items[0], Events{})
	if outc.Kind != OutcomeDone {
		t.Fatalf("outcome %v: %v", outc.Kind, outc.Err)
	}
	if want := filepath.Join(out, "Outer", "Inner", "data.bin"); outc.Result.Path != want {
		t.Errorf("saved to %s, want %s (the inner folder inside the outer one)", outc.Result.Path, want)
	}
	got, err := os.ReadFile(outc.Result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, e.body) {
		t.Fatal("content is corrupt")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ranges < 5 {
		t.Errorf("%d range requests, want a probe and 4 segments: without a server validator the SHA-256 should allow them", e.ranges)
	}
	if e.ifRanges != 0 {
		t.Errorf("%d requests carried If-Range; the server gave nothing to compare it with", e.ifRanges)
	}
	if e.pages != 1 {
		t.Errorf("the file page was read %d times, want once (one attempt)", e.pages)
	}
}
