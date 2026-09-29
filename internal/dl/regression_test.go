package dl

// This file holds regression tests for bugs found in code review. Each test
// corresponds to a finding and keeps the fix from being reverted.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// FINDING 1. The sneakiest inconsistency: the `.part` was deleted but the
// `.part.state` remains and carries a saved sha256 state. A naive resume
// opens a ZERO hole of offset bytes at the start of the file; since the hash
// state comes from the state file the sha256 check PASSES and the corrupt file
// is written under its final name.
//
// Measured before the fix: a file with 20000 zero bytes was written as "OK"
// and Download returned nil.
func TestMissingPartWithStateMustNotCorrupt(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	part := filepath.Join(out, "data.bin.part")

	h := sha256.New()
	h.Write(payload[:20000])
	hs, err := h.(interface{ MarshalBinary() ([]byte, error) }).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	st := State{
		Offset: 20000, Validator: `"v1"`, ValidatorType: ValidatorETag,
		TotalSize: int64(len(payload)), SHA256State: hs,
	}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(part+".state", data, 0o644); err != nil {
		t.Fatal(err)
	}
	// The .part is DELIBERATELY missing.

	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("downloading from scratch must not fail: %v", err)
	}
	got, rerr := os.ReadFile(filepath.Join(out, "data.bin"))
	if rerr != nil {
		t.Fatalf("file missing: %v", rerr)
	}
	if !bytes.Equal(got, payload) {
		zeros := bytes.Count(got[:20000], []byte{0})
		t.Fatalf("CORRUPT FILE: %d zeros in the first 20000 bytes", zeros)
	}
}

// FINDING 2. If the `.part` and state stay on disk after a hash mismatch,
// every run repeats the same failure and the item can't be recovered without
// deleting it by hand.
func TestSHA256MismatchCleansUpForRetry(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)

	bad := testItem(srv.URL+"/data.bin", "data.bin")
	bad.SHA256 = strings.Repeat("00", 32)
	d := &Downloader{Client: srv.Client()}
	if _, err := d.Download(context.Background(), out, bad); !errors.Is(err, ErrSHA256Mismatch) {
		t.Fatalf("expected ErrSHA256Mismatch: %v", err)
	}
	part := filepath.Join(out, "data.bin.part")
	for _, f := range []string{part, part + ".state"} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s was not cleaned up; the next run hits the same failure", filepath.Base(f))
		}
	}

	// It must be retryable in the same folder with the right hash.
	good := testItem(srv.URL+"/data.bin", "data.bin")
	good.SHA256 = payloadSHA()
	d2 := &Downloader{Client: srv.Client()}
	if _, err := d2.Download(context.Background(), out, good); err != nil {
		t.Fatalf("cannot retry after cleanup: %v", err)
	}
}

// FINDING 3. user_agent and Item.Headers must be applied to the transfer
// request too. Applying them only to API calls is a silent half
// implementation, and on bunkr the item page Referer is mandatory on the
// transfer.
func TestUserAgentAndHeadersAppliedToTransfer(t *testing.T) {
	var gotUA, gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotReferer = r.Header.Get("Referer")
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	d := &Downloader{Client: srv.Client(), UserAgent: "siphon/test"}
	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.Headers = map[string]string{"Referer": "https://example.test/u/abc"}
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), tempDir(t), it); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if gotUA != "siphon/test" {
		t.Errorf("User-Agent = %q, not applied to the transfer", gotUA)
	}
	if gotReferer != "https://example.test/u/abc" {
		t.Errorf("Referer = %q; Item.Headers not applied to the transfer", gotReferer)
	}
}

// FINDING 5. With a classifier, a 403 must not count as "signed URL expired"
// and must NOT trigger re-resolution. Otherwise the tool would re-resolve and
// retry while rate limited, deepening the limit with its own hands.
func TestClassifierPreventsReresolveOn403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"value":"transfer_limit_exceeded","message":"limit"}`))
	}))
	t.Cleanup(srv.Close)

	sentinel := errors.New("rate limit")
	calls := 0
	d := &Downloader{
		Client:    srv.Client(),
		Classify:  func(resp *http.Response, body []byte) error { return sentinel },
		Reresolve: func(context.Context, string) (site.Item, error) { calls++; return site.Item{}, nil },
	}
	_, err := d.Download(context.Background(), tempDir(t), testItem(srv.URL+"/x", "data.bin"))
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the classifier's error, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("Reresolve called %d times; re-resolving while rate limited deepens the limit", calls)
	}
}

// When the classifier returns nil (unrecognized body) the old behavior must be kept.
func TestClassifierReturningNilFallsBackToExpired(t *testing.T) {
	good := rangeServer(t, `"v1"`, nil)
	expired := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(expired.Close)

	calls := 0
	d := &Downloader{
		Client:   good.Client(),
		Classify: func(resp *http.Response, body []byte) error { return nil },
		Reresolve: func(context.Context, string) (site.Item, error) {
			calls++
			return testItem(good.URL+"/data.bin", "data.bin"), nil
		},
	}
	it := testItem(expired.URL+"/x", "data.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), tempDir(t), it); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Reresolve called %d times, want 1", calls)
	}
}

// FINDING 9. No Content-Length and no hash: a truncated body must not count
// as "complete". The resolver-reported size kicks in as a fallback.
func TestTruncatedBodyCaughtByItemSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// NO Content-Length and the body is deliberately short.
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload[:1000])
	}))
	t.Cleanup(srv.Close)

	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/data.bin", "data.bin") // Size = len(payload)
	if _, err := d.Download(context.Background(), out, it); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("expected ErrIncomplete, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(out, "data.bin")); serr == nil {
		t.Fatal("a truncated body was written under the final name")
	}
}

// If the resolver doesn't know the size (Size -1) no check is possible; the
// old behavior is kept. This also shows the decision not to resume chunked
// responses is untouched.
func TestUnknownSizeChunkedStillCompletes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.Size = -1
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), tempDir(t), it); err != nil {
		t.Fatalf("Download: %v", err)
	}
}

// FINDING 8. A generated "(N)" name can itself collide; in that case the
// second item fell into the "already exists" branch and was reported OK
// without ever being downloaded.
func TestGeneratedDedupNameIsAlsoDeduped(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	// All three collide: the name b would generate ("same (2).bin") equals c's plain name.
	a := testItem(srv.URL+"/data.bin", "same.bin")
	a.Index = 0
	a.SourcePage = "https://example.test/u/a"
	b := testItem(srv.URL+"/data.bin", "same.bin")
	b.Index = 1
	b.SourcePage = "https://example.test/u/b"
	c := testItem(srv.URL+"/data.bin", "same (2).bin")
	c.Index = 2
	c.SourcePage = "https://example.test/u/c"

	for i, it := range []site.Item{a, b, c} {
		if _, err := d.Download(context.Background(), out, it); err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("%d files created, want 3: %v", len(entries), names)
	}
}
