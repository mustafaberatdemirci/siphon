package dl

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// pathServer takes byte ranges the way mega's storage servers do: in the URL
// path ("/f/<start>-<end>", end inclusive, or "/f/<start>" to the end), and
// answers 200 with exactly that range. It deliberately ignores the Range
// header, so a test fails if the downloader relies on it. No ETag, no
// Last-Modified.
type pathServer struct {
	srv  *httptest.Server
	body []byte

	mu         sync.Mutex
	paths      []string
	withRange  int  // requests that carried a Range header
	ignorePath bool // behave like a server that doesn't take ranges in the path
	status     int  // if non-zero, every request gets this status
	etag       string
	holdFirst  chan struct{} // if set, the first body stops after holdBytes
	holdBytes  int
}

func newPathServer(t *testing.T, size int) *pathServer {
	t.Helper()
	s := &pathServer{body: make([]byte, size)}
	if _, err := rand.Read(s.body); err != nil {
		t.Fatal(err)
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		s.mu.Lock()
		if s.holdFirst != nil {
			select {
			case <-s.holdFirst:
			default:
				close(s.holdFirst)
			}
		}
		s.mu.Unlock()
		s.srv.Close()
	})
	return s
}

func (s *pathServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.Path)
	if r.Header.Get("Range") != "" {
		s.withRange++
	}
	status, ignore, etag := s.status, s.ignorePath, s.etag
	hold := s.holdFirst
	s.holdFirst = nil
	holdBytes := s.holdBytes
	s.mu.Unlock()

	if status != 0 {
		http.Error(w, "refused", status)
		return
	}
	start, end := int64(0), int64(len(s.body))
	if rest := strings.TrimPrefix(r.URL.Path, "/f/"); rest != r.URL.Path && !ignore {
		a, b, dash := strings.Cut(rest, "-")
		start, _ = strconv.ParseInt(a, 10, 64)
		if dash && b != "" {
			e, _ := strconv.ParseInt(b, 10, 64)
			end = e + 1
		}
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.Header().Set("Content-Length", fmt.Sprint(end-start))
	w.WriteHeader(http.StatusOK)
	if hold != nil {
		_, _ = w.Write(s.body[start : start+int64(holdBytes)])
		w.(http.Flusher).Flush()
		<-hold
		return
	}
	_, _ = w.Write(s.body[start:end])
}

func (s *pathServer) requests() (paths []string, withRange int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...), s.withRange
}

func pathRange(u string, start, end int64) string { return fmt.Sprintf("%s/%d-%d", u, start, end-1) }

func pathItem(s *pathServer, name string) site.Item {
	return site.Item{URL: s.srv.URL + "/f", SourcePage: "https://example.test/u/" + name, Filename: name, Size: int64(len(s.body))}
}

// A decoded file (mega) fetched over several connections with the ranges in
// the URL: no request carries a Range header, and plaintext ends up on disk.
func TestRangeURLSegmentedDecoded(t *testing.T) {
	s := newPathServer(t, 1<<20)
	plain := append([]byte(nil), s.body...)
	xorAt(s.body, 0) // what goes over the wire
	sum := sha256.Sum256(plain)
	d := &Downloader{
		Client: s.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10, ChunkSize: 256 << 10,
		RangeURL: pathRange,
		Decode: func(site.Item, int64, []byte, io.Reader) (site.DecodedStream, error) {
			return nil, errors.New("single-stream decoder used")
		},
		DecodeRange: func(_ site.Item, off int64, r io.Reader) (io.Reader, error) {
			return &xorRangeReader{r: r, pos: off}, nil
		},
		NewVerifier: func(site.Item) (site.Verifier, error) {
			h := sha256.New()
			return &shaVerifier{h: h, w: h, want: hex.EncodeToString(sum[:])}, nil
		},
	}
	res, err := d.Download(context.Background(), tempDir(t), pathItem(s, "enc.bin"))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, plain) {
		t.Fatal("content is corrupt")
	}
	paths, withRange := s.requests()
	if withRange != 0 {
		t.Errorf("%d requests carried a Range header; the ranges belong in the URL", withRange)
	}
	want := map[string]bool{"/f/0-0": true, "/f/0-262143": true, "/f/262144-524287": true, "/f/524288-786431": true, "/f/786432-1048575": true}
	for _, p := range paths {
		if !want[p] {
			t.Errorf("unexpected request %q", p)
		}
		delete(want, p)
	}
	if len(want) != 0 {
		t.Errorf("never requested: %v (got %v)", want, paths)
	}
}

// A server that ignores the range in the URL sends the whole file to the
// probe; the downloader must notice and fall back to a single stream
// instead of writing the file's start at every chunk's position.
func TestRangeURLIgnoredFallsBackToSingleStream(t *testing.T) {
	s := newPathServer(t, 512<<10)
	s.ignorePath = true
	s.etag = `"v1"`
	d := &Downloader{Client: s.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10, RangeURL: pathRange}
	res, err := d.Download(context.Background(), tempDir(t), pathItem(s, "whole.bin"))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, s.body) {
		t.Fatal("content is corrupt")
	}
	if paths, _ := s.requests(); len(paths) != 2 {
		t.Errorf("requests %v; want the probe and one full download", paths)
	}
}

// A single-stream download continues through the URL too: after an
// interruption the second run asks for "/f/<offset>-<end>", not the file
// from the start.
func TestRangeURLSingleStreamResume(t *testing.T) {
	s := newPathServer(t, 300<<10)
	s.etag = `"v1"`
	s.holdFirst = make(chan struct{})
	hold := s.holdFirst
	s.holdBytes = 100 << 10
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), RangeURL: pathRange}
	it := pathItem(s, "resume.bin")
	sum := sha256.Sum256(s.body)
	it.SHA256 = hex.EncodeToString(sum[:])

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	if _, err := d.Download(ctx, out, it); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	close(hold)

	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, s.body) {
		t.Fatal("content corrupted after resume")
	}
	paths, withRange := s.requests()
	if len(paths) != 2 || paths[0] != "/f" || !strings.HasPrefix(paths[1], "/f/") || strings.HasPrefix(paths[1], "/f/0-") {
		t.Errorf("requests %v; want the whole file, then the rest from where it stopped", paths)
	}
	if withRange != 0 {
		t.Errorf("%d requests carried a Range header", withRange)
	}
	if _, err := os.Stat(filepath.Join(out, "resume.bin.part.state")); !os.IsNotExist(err) {
		t.Error("state left behind")
	}
}

// CheckAccess is the queue's quota probe: one byte, classified like a download.
func TestCheckAccess(t *testing.T) {
	s := newPathServer(t, 4096)
	quota := &site.QuotaError{Err: errors.New("quota (fake 509)")}
	d := &Downloader{
		Client: s.srv.Client(), RangeURL: pathRange,
		Classify: func(resp *http.Response, _ []byte) error {
			if resp.StatusCode == 509 {
				return quota
			}
			return nil
		},
	}
	it := pathItem(s, "probe.bin")

	if err := d.CheckAccess(context.Background(), it); err != nil {
		t.Fatalf("open file: %v", err)
	}
	if paths, _ := s.requests(); len(paths) != 1 || paths[0] != "/f/0-0" {
		t.Errorf("the probe asked for %v; want one byte (/f/0-0)", paths)
	}

	s.mu.Lock()
	s.status = 509
	s.mu.Unlock()
	if err := d.CheckAccess(context.Background(), it); !errors.Is(err, quota) {
		t.Errorf("509: %v, want the site's quota error", err)
	}

	s.mu.Lock()
	s.status = http.StatusForbidden
	s.mu.Unlock()
	if err := d.CheckAccess(context.Background(), it); !IsURLExpired(err) {
		t.Errorf("403: %v, want an expired URL", err)
	}
}
