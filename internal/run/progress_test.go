package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	snet "github.com/mustafaberatdemirci/siphon/internal/net"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// droppingServer serves a file and breaks every connection after step more
// bytes. With ranges honored the file still comes, a step at a time; with
// honor false every answer is the file from its start.
func droppingServer(t *testing.T, body []byte, step int, honor bool, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		start := 0
		if rng := r.Header.Get("Range"); honor && strings.HasPrefix(rng, "bytes=") {
			start, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"))
		}
		w.Header().Set("ETag", `"v1"`)
		if start > 0 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
			w.Header().Set("Content-Length", strconv.Itoa(len(body)-start))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
		}
		end := start + step
		if end >= len(body) {
			_, _ = w.Write(body[start:])
			return
		}
		_, _ = w.Write(body[start:end])
		w.(http.Flusher).Flush()
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func quickWorker(r site.Resolver, client *http.Client, attempts int) *Worker {
	cfg := site.SiteConfig{Name: "fake", MaxRetries: attempts}.WithDefaults()
	w := NewWorker(r, cfg, client, Events{})
	w.Policy.Backoff = snet.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond}
	return w
}

// WHAT THE USER WOULD EXPERIENCE: a large file over one connection that
// drops now and then (a VPN reconnecting). Every drop used to cost one of
// five attempts, and after ten minutes of downloading the first drop ended
// the job. A file that keeps coming is not given up: here the connection
// drops ten times, with three attempts allowed.
func TestDownloadThatKeepsProgressingOutlastsItsAttempts(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KB
	var hits atomic.Int64
	srv := droppingServer(t, body, len(body)/10, true, &hits)
	out := tempDir(t)
	w := quickWorker(&fakeResolver{}, srv.Client(), 3)

	it := site.Item{URL: srv.URL + "/big.bin", SourcePage: "https://fake.test/f/big", Filename: "big.bin", Size: int64(len(body))}
	o := w.DownloadItem(context.Background(), out, nil, it, Events{})
	if o.Kind != OutcomeDone {
		t.Fatalf("outcome %v: %v", o.Kind, o.Err)
	}
	got, err := os.ReadFile(o.Result.Path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("content is wrong (%v)", err)
	}
	if n := hits.Load(); n < 10 {
		t.Errorf("%d requests; the file should have taken at least 10", n)
	}
}

// The other side of it: a download that starts over at every drop and never
// gets further than before must still run out of attempts, not spin.
func TestDownloadThatNeverGetsFurtherStillGivesUp(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789abcdef"), 4096)
	var hits atomic.Int64
	srv := droppingServer(t, body, len(body)/10, false, &hits)
	w := quickWorker(&fakeResolver{}, srv.Client(), 3)

	it := site.Item{URL: srv.URL + "/big.bin", SourcePage: "https://fake.test/f/big", Filename: "big.bin", Size: int64(len(body))}
	done := make(chan Outcome, 1)
	go func() { done <- w.DownloadItem(context.Background(), tempDir(t), nil, it, Events{}) }()
	select {
	case o := <-done:
		if o.Kind != OutcomeFailed || !errors.Is(o.Err, snet.ErrExhausted) {
			t.Fatalf("outcome %v: %v", o.Kind, o.Err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the download was retried without end")
	}
	// The first attempt got further than nothing; three more may fail.
	if n := hits.Load(); n > 12 {
		t.Errorf("%d requests for a file that never got further", n)
	}
}
