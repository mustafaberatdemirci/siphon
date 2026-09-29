package dl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

var payload = bytes.Repeat([]byte("siphon-data-block."), 4096) // ~72 KB

func payloadSHA() string {
	s := sha256.Sum256(payload)
	return hex.EncodeToString(s[:])
}

type capture struct {
	mu       []http.Header
	statuses []int
}

// rangeServer leaves Range/If-Range semantics to Go's own ServeContent: real
// behavior is tested instead of a hand-written imitation.
func rangeServer(t *testing.T, etag string, cap *capture) *httptest.Server {
	t.Helper()
	modtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cap != nil {
			cap.mu = append(cap.mu, r.Header.Clone())
		}
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		http.ServeContent(w, r, "data.bin", modtime, bytes.NewReader(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testItem(url, name string) site.Item {
	return site.Item{
		URL:        url,
		SourcePage: "https://example.test/u/abc",
		Filename:   name,
		Size:       int64(len(payload)),
	}
}

func readState(t *testing.T, path string) State {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read state: %v", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("could not decode state: %v", err)
	}
	return st
}

func TestFreshDownloadWritesFileAndCleansUp(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("Download: %v", err)
	}

	final := filepath.Join(out, "data.bin")
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("final file missing: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("wrong content: %d bytes, want %d", len(got), len(payload))
	}
	// A partial file never sits on disk under the final name; .part and .state are cleaned up.
	for _, leftover := range []string{final + ".part", final + ".part.state"} {
		if _, err := os.Stat(leftover); err == nil {
			t.Errorf("leftover file: %s", leftover)
		}
	}
}

func TestSHA256MismatchDoesNotRename(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.SHA256 = strings.Repeat("00", 32)
	_, err := d.Download(context.Background(), out, it)
	if !errors.Is(err, ErrSHA256Mismatch) {
		t.Fatalf("expected ErrSHA256Mismatch, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "data.bin")); err == nil {
		t.Fatal("the file was written under the final name despite a hash mismatch")
	}
}

// slowServer sends the first N bytes and then blocks. It imitates the Ctrl+C scenario.
func slowServer(t *testing.T, firstChunk int, release <-chan struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload[:firstChunk])
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestInterruptThenResumeProducesCorrectHash(t *testing.T) {
	release := make(chan struct{})
	slow := slowServer(t, 20000, release)
	out := tempDir(t)

	ctx, cancel := context.WithCancel(context.Background())
	d := &Downloader{Client: slow.Client()}
	it := testItem(slow.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()

	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err := d.Download(ctx, out, it)
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	part := filepath.Join(out, "data.bin.part")
	statePath := part + ".state"
	st := readState(t, statePath)
	if st.Offset <= 0 || st.Offset >= int64(len(payload)) {
		t.Fatalf("unexpected offset: %d", st.Offset)
	}
	if st.ValidatorType != ValidatorETag || st.Validator != `"v1"` {
		t.Fatalf("wrong validator: %+v", st)
	}
	if len(st.SHA256State) == 0 {
		t.Fatal("sha256 state not saved; resume would have to rehash from the start")
	}
	fi, err := os.Stat(part)
	if err != nil || fi.Size() != st.Offset {
		t.Fatalf(".part size doesn't match the state: %v", err)
	}

	// Continue in the same folder with a working server.
	cap := &capture{}
	srv := rangeServer(t, `"v1"`, cap)
	d2 := &Downloader{Client: srv.Client()}
	it2 := testItem(srv.URL+"/data.bin", "data.bin")
	it2.SHA256 = payloadSHA()
	if _, err := d2.Download(context.Background(), out, it2); err != nil {
		t.Fatalf("resume: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(out, "data.bin"))
	if err != nil {
		t.Fatalf("final file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("content corrupted after resume")
	}
	if len(cap.mu) == 0 || cap.mu[0].Get("Range") == "" {
		t.Fatal("no Range header on the resume request")
	}
	if cap.mu[0].Get("If-Range") != `"v1"` {
		t.Errorf("If-Range = %q, expected the strong ETag", cap.mu[0].Get("If-Range"))
	}
}

// If the name is at the limit (255 - len(".part.state")) the state's TEMP file
// must fit into the component limit too. Previously ".part.state.tmp" reached
// 259 units, the write failed silently and files with long names could never
// be resumed.
func TestStateSavedForMaxLengthName(t *testing.T) {
	release := make(chan struct{})
	slow := slowServer(t, 20000, release)
	out := tempDir(t)

	ctx, cancel := context.WithCancel(context.Background())
	d := &Downloader{Client: slow.Client()}
	it := testItem(slow.URL+"/data.bin", strings.Repeat("u", 400)+".bin")
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	res, err := d.Plan(out, it)
	if err != nil {
		t.Fatal(err)
	}
	if n := utf16Len(filepath.Base(res)) + utf16Len(stateSuffix); n != MaxComponentUTF16 {
		t.Fatalf("test setup: state name is %d units, expected exactly the limit (%d)", n, MaxComponentUTF16)
	}
	_, err = d.Download(ctx, out, it)
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled: %v", err)
	}
	st := readState(t, res+stateSuffix)
	if st.Offset <= 0 {
		t.Fatalf("state did not advance for a file with a long name: %+v", st)
	}
}

// If a file already exists under the final name and its size DOESN'T MATCH
// the item's, it is a different file (another album with the same name, the
// user's own file). Counting it as "already downloaded" and returning success
// is a silent failure; the error must be visible. If the size matches or is
// unknown, the old behavior applies: it is skipped.
func TestExistingFinalWithDifferentSizeIsAnError(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	final := filepath.Join(out, "data.bin")
	if err := os.WriteFile(final, []byte("a different file"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := &Downloader{Client: srv.Client()}
	_, err := d.Download(context.Background(), out, testItem(srv.URL+"/data.bin", "data.bin"))
	if err == nil {
		t.Fatal("an existing file with a different size counted as a success")
	}
	var rt interface{ Retryable() bool }
	if errors.As(err, &rt) && rt.Retryable() {
		t.Error("a name collision must not be retryable")
	}
	if got, _ := os.ReadFile(final); string(got) != "a different file" {
		t.Error("the existing file was touched")
	}

	// With an unknown size (the resolver said -1) there is nothing to compare.
	unknown := testItem(srv.URL+"/data.bin", "data.bin")
	unknown.Size = -1
	if _, err := d.Download(context.Background(), out, unknown); err != nil {
		t.Errorf("with an unknown size the existing file should have been skipped: %v", err)
	}
}

func TestWeakETagFallsBackToLastModified(t *testing.T) {
	release := make(chan struct{})
	// Weak ETag: W/ prefix. CANNOT be used in If-Range (RFC 7232).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"weak"`)
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2026 03:04:05 GMT")
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload[:10000])
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(srv.Close)

	out := tempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	d := &Downloader{Client: srv.Client()}
	_, _ = d.Download(ctx, out, testItem(srv.URL+"/data.bin", "data.bin"))
	close(release)

	st := readState(t, filepath.Join(out, "data.bin.part.state"))
	if st.ValidatorType != ValidatorLastModified {
		t.Fatalf("validator_type = %q, should fall back to Last-Modified with a weak ETag", st.ValidatorType)
	}
	if strings.HasPrefix(st.Validator, "W/") {
		t.Fatal("a weak ETag was saved as the validator; that produces corrupt files")
	}
}

func TestNoValidatorMeansNoResumeAttempt(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Neither ETag nor Last-Modified.
		w.Header()["Last-Modified"] = nil
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload[:10000])
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(srv.Close)

	out := tempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	d := &Downloader{Client: srv.Client()}
	_, _ = d.Download(ctx, out, testItem(srv.URL+"/data.bin", "data.bin"))
	close(release)

	st := readState(t, filepath.Join(out, "data.bin.part.state"))
	if st.resumable() {
		t.Fatalf("marked resumable without a validator: %+v", st)
	}

	cap := &capture{}
	srv2 := rangeServer(t, "", cap)
	d2 := &Downloader{Client: srv2.Client()}
	if _, err := d2.Download(context.Background(), out, testItem(srv2.URL+"/data.bin", "data.bin")); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if cap.mu[0].Get("Range") != "" {
		t.Fatal("Range was sent without a validator; parts of two different files could be glued together")
	}
	got, _ := os.ReadFile(filepath.Join(out, "data.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("content corrupted after downloading from scratch")
	}
}

func TestServerReturns200OnResumeResetsFile(t *testing.T) {
	out := tempDir(t)
	part := filepath.Join(out, "data.bin.part")
	// Set up a corrupt .part + state by hand: the server will ignore Range and return 200.
	if err := os.WriteFile(part, []byte("old-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := State{Offset: 11, Validator: `"old"`, ValidatorType: ValidatorETag, TotalSize: int64(len(payload))}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(part+".state", data, 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately ignore Range.
		w.Header().Set("ETag", `"new"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(out, "data.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("old content was not cleared after the 200")
	}
}

func TestRange416WhenAlreadyComplete(t *testing.T) {
	out := tempDir(t)
	part := filepath.Join(out, "data.bin.part")
	// The .part is full size but the rename failed.
	if err := os.WriteFile(part, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	h.Write(payload)
	st := State{
		Offset: int64(len(payload)), Validator: `"v1"`, ValidatorType: ValidatorETag,
		TotalSize: int64(len(payload)),
	}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(part+".state", data, 0o644); err != nil {
		t.Fatal(err)
	}

	srv := rangeServer(t, `"v1"`, nil) // ServeContent answers this Range with 416
	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("416/complete path: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(out, "data.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("the complete .part was not renamed")
	}
}

func TestRange416WhenPartIsCorruptResets(t *testing.T) {
	out := tempDir(t)
	part := filepath.Join(out, "data.bin.part")
	// The state doesn't say "complete": offset != total. Here 416 means "corrupt".
	if err := os.WriteFile(part, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	st := State{
		Offset: int64(len(payload)), Validator: `"v1"`, ValidatorType: ValidatorETag,
		TotalSize: int64(len(payload)) + 999, // wrong total
	}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(part+".state", data, 0o644); err != nil {
		t.Fatal(err)
	}

	srv := rangeServer(t, `"v1"`, nil)
	d := &Downloader{Client: srv.Client()}
	_, err := d.Download(context.Background(), out, testItem(srv.URL+"/data.bin", "data.bin"))
	if err == nil {
		t.Fatal("expected an error for a corrupt .part; 416 does not mean 'complete'")
	}
	if _, serr := os.Stat(filepath.Join(out, "data.bin")); serr == nil {
		t.Fatal("a corrupt .part was written under the final name")
	}
	if fi, serr := os.Stat(part); serr == nil && fi.Size() != 0 {
		t.Fatalf(".part was not reset: %d bytes", fi.Size())
	}
}

func TestChunkedResponseStillUsesPartFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// NO Content-Length: chunked. Resume must not be attempted but .part must be used.
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("chunked: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(out, "data.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("chunked content is corrupt")
	}
}

func TestDuplicateFilenameGetsDeterministicSuffix(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	// Different items carry DIFFERENT SourcePages; name ownership is kept by identity.
	a := testItem(srv.URL+"/data.bin", "same.bin")
	a.Index = 0
	a.SourcePage = "https://example.test/u/a"
	b := testItem(srv.URL+"/data.bin", "same.bin")
	b.Index = 4
	b.SourcePage = "https://example.test/u/b"

	if _, err := d.Download(context.Background(), out, a); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := d.Download(context.Background(), out, b); err != nil {
		t.Fatalf("second: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "same.bin")); err != nil {
		t.Error("the first file should have been written under the plain name")
	}
	// The suffix derives from Index, so it is deterministic across runs.
	if _, err := os.Stat(filepath.Join(out, "same (5).bin")); err != nil {
		t.Errorf("the second file should be 'same (5).bin': %v", err)
	}
}

func TestExpiredSignedURLTriggersReresolveOnce(t *testing.T) {
	good := rangeServer(t, `"v1"`, nil)
	expired := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(expired.Close)

	calls := 0
	out := tempDir(t)
	d := &Downloader{
		Client: good.Client(),
		Reresolve: func(ctx context.Context, sourcePage string) (site.Item, error) {
			calls++
			return testItem(good.URL+"/data.bin", "data.bin"), nil
		},
	}
	it := testItem(expired.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Reresolve called %d times, want 1", calls)
	}
	got, _ := os.ReadFile(filepath.Join(out, "data.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("content corrupted after re-resolution")
	}
}

func TestExpiredURLWithoutReresolverIsPermanent(t *testing.T) {
	expired := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	t.Cleanup(expired.Close)
	d := &Downloader{Client: expired.Client()}
	_, err := d.Download(context.Background(), tempDir(t), testItem(expired.URL+"/x", "data.bin"))
	if err == nil {
		t.Fatal("without Reresolve a 410 must be a permanent error")
	}
}

// If the .part is shorter than the state, resume opens a hole at the start of
// the file. The only safe behavior is starting over, and the result must NOT
// BE CORRUPT.
func TestPartShorterThanStateRestartsCleanly(t *testing.T) {
	out := tempDir(t)
	part := filepath.Join(out, "data.bin.part")
	if err := os.WriteFile(part, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := State{Offset: 9999, Validator: `"v1"`, ValidatorType: ValidatorETag, TotalSize: int64(len(payload))}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(part+".state", data, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := rangeServer(t, `"v1"`, nil)
	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("downloading from scratch failed: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(out, "data.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("content is corrupt")
	}
}

func TestParseContentRange(t *testing.T) {
	cases := []struct {
		in         string
		start, tot int64
		wantErr    bool
	}{
		{"bytes 100-199/1234", 100, 1234, false},
		{"bytes 0-0/1", 0, 1, false},
		{"bytes 500-999/*", 500, -1, false},
		{"pages 1-2/3", 0, 0, true},
		{"bytes broken", 0, 0, true},
		{"", 0, 0, true},
	}
	for _, c := range cases {
		s, tt, err := parseContentRange(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseContentRange(%q) expected an error", c.in)
			}
			continue
		}
		if err != nil || s != c.start || tt != c.tot {
			t.Errorf("parseContentRange(%q) = %d,%d,%v", c.in, s, tt, err)
		}
	}
}

// --- Name ownership ---

// If the SAME item asks for a name a second time it must get the same name
// back: without this, pause/resume veers off to a new name with a "(N)"
// suffix and the half .part is orphaned.
func TestClaimIsIdempotentForSameItem(t *testing.T) {
	d := &Downloader{}
	a := site.Item{SourcePage: "https://s.test/f/a", Filename: "video.mp4"}
	first := d.claim("out", a)
	second := d.claim("out", a)
	if first != "video.mp4" || second != first {
		t.Fatalf("different names for the same item: %q, %q", first, second)
	}
	// A DIFFERENT item asking for the same name must get a suffix.
	b := site.Item{SourcePage: "https://s.test/f/b", Filename: "video.mp4", Index: 1}
	if got := d.claim("out", b); got == "video.mp4" {
		t.Fatalf("a different item got the same name: %q", got)
	}
}

// Plan must tell the path Download will use, in advance and consistently.
func TestPlanMatchesDownloadPath(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}
	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.Dir = "Album"

	planned, err := d.Plan(out, it)
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != planned {
		t.Fatalf("Plan said %q, Download wrote %q", planned, res.Path)
	}
}
