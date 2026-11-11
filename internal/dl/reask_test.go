package dl

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// signedServer answers the way bunkr's *.cdn.cr file servers were measured
// to (2026-11-11): the first request made with a signature it hasn't seen
// gets 200 and the whole file, whatever its Range; the same URL asked again
// gets the range. cutAfter > 0 breaks the first answer's connection after
// that many bytes (the interrupted download the test resumes).
type signedServer struct {
	*httptest.Server
	mu       sync.Mutex
	seen     map[string]bool
	requests []string // "<status> <Range header>"
	cutAfter int
	// forget makes every request a first one: the range is never honored.
	forget bool
}

func newSignedServer(t *testing.T, etag string) *signedServer {
	t.Helper()
	s := &signedServer{seen: map[string]bool{}}
	modtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sig := r.URL.Query().Get("token")
		s.mu.Lock()
		first := !s.seen[sig] || s.forget
		s.seen[sig] = true
		cut := s.cutAfter
		s.cutAfter = 0
		s.mu.Unlock()

		w.Header().Set("ETag", etag)
		if !first {
			s.note(http.StatusPartialContent, r)
			http.ServeContent(w, r, "data.bin", modtime, bytes.NewReader(payload))
			return
		}
		s.note(http.StatusOK, r)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		if cut > 0 {
			_, _ = w.Write(payload[:cut])
			w.(http.Flusher).Flush()
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *signedServer) note(status int, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, fmt.Sprintf("%d %s", status, r.Header.Get("Range")))
	s.mu.Unlock()
}

func (s *signedServer) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// signEachAttempt is a PrepareURL that takes a new signature every time, as
// bunkr's does.
func signEachAttempt() func(context.Context, string) (string, error) {
	n := 0
	return func(_ context.Context, raw string) (string, error) {
		n++
		return fmt.Sprintf("%s?token=%d", raw, n), nil
	}
}

func noReaskWait(t *testing.T) {
	t.Helper()
	old := rangeReaskWait
	rangeReaskWait = 0
	t.Cleanup(func() { rangeReaskWait = old })
}

// The resume request's first answer is the whole file, with the ETag the
// .part was saved under: the file didn't change, the server ignored the
// range. The download must go on from the .part, not start over.
func TestResumeAsksAgainWhenTheRangeIsIgnored(t *testing.T) {
	noReaskWait(t)
	srv := newSignedServer(t, `"abc-12"`)
	srv.cutAfter = 30000
	out := tempDir(t)
	var logs []string
	d := &Downloader{
		Client:     srv.Client(),
		PrepareURL: signEachAttempt(),
		Logf:       func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()

	_, err := d.Download(context.Background(), out, it)
	if err == nil {
		t.Fatal("the cut connection gave no error")
	}
	reached, ok := Reached(err)
	st := readState(t, filepath.Join(out, "data.bin.part.state"))
	if st.Offset <= 0 || st.Offset >= int64(len(payload)) {
		t.Fatalf("offset after the cut = %d", st.Offset)
	}
	if !ok || reached != st.Offset {
		t.Errorf("Reached = %d, %v; want the %d bytes saved", reached, ok, st.Offset)
	}

	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(out, "data.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("content after the resume is wrong (%v)", err)
	}
	asked := srv.asked()
	want := fmt.Sprintf("206 bytes=%d-", st.Offset)
	if len(asked) != 3 || asked[2] != want {
		t.Errorf("requests = %q; want the cut one, the ignored range, then %q", asked, want)
	}
	for _, l := range logs {
		if strings.Contains(l, "from scratch") {
			t.Errorf("the file was started over: %s", l)
		}
	}
}

// A server that never honors the range still gets its file: after the
// re-asks the download starts over, as it always did.
func TestRangeNeverHonoredStartsOverAfterReasks(t *testing.T) {
	noReaskWait(t)
	srv := newSignedServer(t, `"abc-12"`)
	srv.cutAfter = 30000
	out := tempDir(t)
	d := &Downloader{Client: srv.Client(), PrepareURL: signEachAttempt()}
	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err == nil {
		t.Fatal("the cut connection gave no error")
	}
	srv.mu.Lock()
	srv.forget = true
	srv.mu.Unlock()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("second download: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(out, "data.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("content is wrong after starting over")
	}
	if n := len(srv.asked()); n != 1+1+rangeReasks {
		t.Errorf("%d requests; want the cut one, the resume and %d re-asks", n, rangeReasks)
	}
}

// An attempt that added nothing to the .part reports no progress.
func TestReachedOnlyWhenBytesWereAdded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusBadGateway)
	}))
	defer srv.Close()
	d := &Downloader{Client: srv.Client()}
	_, err := d.Download(context.Background(), tempDir(t), testItem(srv.URL+"/data.bin", "data.bin"))
	if err == nil {
		t.Fatal("no error")
	}
	if n, ok := Reached(err); ok {
		t.Errorf("Reached = %d for an attempt that got no bytes", n)
	}
}

// "HTTP 503" didn't say which of a site's servers wanted fewer connections.
func TestTooManyConnectionsNamesTheServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	d := &Downloader{Client: srv.Client()}
	_, err := d.Download(context.Background(), tempDir(t), testItem(srv.URL+"/data.bin", "data.bin"))
	if !IsOverloaded(err) || !strings.Contains(err.Error(), "HTTP 503 from 127.0.0.1") {
		t.Fatalf("err = %v, want a 503 naming the server", err)
	}
}
