package dl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// A server that names no version (mediafire's download servers send neither
// ETag nor Last-Modified) still gets several connections when the site told
// us the file's SHA-256: the hash over the finished file catches a mix-up.
// No If-Range goes out: there is nothing the server could compare it with.
func TestKnownSHA256AllowsSegmentsWithoutValidator(t *testing.T) {
	s := newSegServer(t, 1<<20)
	s.noValidator = true
	d := &Downloader{Client: s.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10}
	it := segItem(s, "hashed.bin")
	it.SHA256 = s.sha()

	res, err := d.Download(context.Background(), tempDir(t), it)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, s.body) {
		t.Fatal("content is corrupt")
	}
	if h := atomic.LoadInt32(&s.hits); h != 5 {
		t.Errorf("%d requests, want 5 (1 probe + 4 segments)", h)
	}
	if n := atomic.LoadInt32(&s.ifRanges); n != 0 {
		t.Errorf("%d requests carried If-Range; a hash is not a server validator", n)
	}
}

// noValidatorServer serves payload with Range support but without ETag or
// Last-Modified, and records the Range and If-Range of every request.
func noValidatorServer(t *testing.T) (*httptest.Server, func() (ranges, ifRanges []string)) {
	t.Helper()
	var mu sync.Mutex
	var ranges, ifRanges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		ifRanges = append(ifRanges, r.Header.Get("If-Range"))
		mu.Unlock()
		http.ServeContent(w, r, "data.bin", time.Time{}, bytes.NewReader(payload))
	}))
	t.Cleanup(srv.Close)
	return srv, func() ([]string, []string) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), ranges...), append([]string(nil), ifRanges...)
	}
}

// writeHalfDone leaves the first n bytes of payload as a .part with a state
// whose validator is the given hash.
func writeHalfDone(t *testing.T, out string, n int, hash string) {
	t.Helper()
	part := filepath.Join(out, "data.bin.part")
	if err := os.WriteFile(part, payload[:n], 0o644); err != nil {
		t.Fatal(err)
	}
	st := State{Offset: int64(n), Validator: hash, ValidatorType: ValidatorSHA256, TotalSize: int64(len(payload)), ItemSize: -1}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(part+".state", data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestKnownSHA256ResumesSingleStream(t *testing.T) {
	out := tempDir(t)
	writeHalfDone(t, out, 1000, payloadSHA())
	srv, seen := noValidatorServer(t)
	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()

	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(out, "data.bin")); !bytes.Equal(got, payload) {
		t.Fatal("content is corrupt")
	}
	ranges, ifRanges := seen()
	if len(ranges) != 1 || ranges[0] != "bytes=1000-" {
		t.Errorf("requests asked for %q, want one continuing at byte 1000", ranges)
	}
	if len(ifRanges) != 1 || ifRanges[0] != "" {
		t.Errorf("If-Range = %q, want none", ifRanges)
	}
}

// When the site now reports a different hash, the file changed since the
// .part was written: gluing the rest of the new file onto it would only fail
// the hash check at the end, so it starts over right away.
func TestChangedSHA256RestartsResume(t *testing.T) {
	out := tempDir(t)
	writeHalfDone(t, out, 1000, "0000000000000000000000000000000000000000000000000000000000000000")
	srv, seen := noValidatorServer(t)
	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()

	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(out, "data.bin")); !bytes.Equal(got, payload) {
		t.Fatal("content is corrupt")
	}
	if ranges, _ := seen(); len(ranges) != 1 || ranges[0] != "" {
		t.Errorf("requests asked for %q, want one whole-file request", ranges)
	}
}

// A validator that recognizes an expired session (gofile redirects to its
// web page) gets the item resolved again, and the fresh item's headers are
// used from then on, not only its URL.
func TestLinkExpiredReresolvesAndTakesNewHeaders(t *testing.T) {
	for _, segments := range []int{1, 4} {
		t.Run(fmt.Sprintf("segments=%d", segments), func(t *testing.T) {
			body := bytes.Repeat([]byte("gofile-session."), 64<<10)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Cookie") != "accountToken=fresh" {
					w.Header().Set("Content-Type", "text/html")
					_, _ = w.Write([]byte("<html>the site's front page</html>"))
					return
				}
				http.ServeContent(w, r, "", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), bytes.NewReader(body))
			}))
			t.Cleanup(srv.Close)

			reresolved := 0
			d := &Downloader{
				Client: srv.Client(), Segments: segments, MinSegmentSize: 64 << 10,
				Validate: func(resp *http.Response) error {
					if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
						return fmt.Errorf("%w: a web page came back", site.ErrLinkExpired)
					}
					return nil
				},
				Reresolve: func(_ context.Context, page string) (site.Item, error) {
					reresolved++
					return site.Item{URL: srv.URL + "/f", SourcePage: page, Headers: map[string]string{"Cookie": "accountToken=fresh"}}, nil
				},
			}
			it := site.Item{URL: srv.URL + "/f", SourcePage: "https://example.test/d/x", Filename: "f.bin",
				Size: int64(len(body)), Headers: map[string]string{"Cookie": "accountToken=stale"}}
			res, err := d.Download(context.Background(), tempDir(t), it)
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, body) {
				t.Fatal("content is corrupt (a web page saved as the file?)")
			}
			if reresolved != 1 {
				t.Errorf("re-resolved %d times, want 1", reresolved)
			}
		})
	}
}
