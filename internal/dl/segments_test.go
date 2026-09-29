package dl

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// --- Server for the segmented tests ---
//
// A large payload (the splitting threshold is 8 MiB; tests lower it), Range
// support, a concurrent connection counter and an optional "ignore Range" mode.

type segServer struct {
	t       *testing.T
	srv     *httptest.Server
	body    []byte
	etag    string
	ignore  bool  // ignore Range, always return 200
	hits    int32 // total requests
	ranges  []string
	mu      sync.Mutex
	inFly   int32
	maxFly  int32
	holdAll chan struct{} // if non-nil, bodies wait after the first 4 KB
	limit   int32         // if >0, concurrent requests beyond this get 503
	rejects int32
	// slow: accepted range requests wait this long before writing the body.
	// The local server finishes a 256 KB chunk in microseconds; without the
	// wait no concurrency ever happens and the "limit" test came back empty.
	slow time.Duration
	// noValidator: neither ETag nor Last-Modified (it is not known whether
	// mega's storage servers send one).
	noValidator bool
	// delayFor, if set, delays each range request by an amount chosen from
	// its start offset (one slow connection among fast ones).
	delayFor func(start int64) time.Duration
	// finished lists the Range headers of the requests served, in the order
	// they finished.
	finished []string
}

func newSegServer(t *testing.T, size int) *segServer {
	t.Helper()
	s := &segServer{t: t, etag: `"seg-v1"`}
	s.body = make([]byte, size)
	if _, err := rand.Read(s.body); err != nil {
		t.Fatal(err)
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *segServer) serve(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&s.hits, 1)
	n := atomic.AddInt32(&s.inFly, 1)
	defer atomic.AddInt32(&s.inFly, -1)
	for {
		m := atomic.LoadInt32(&s.maxFly)
		if n <= m || atomic.CompareAndSwapInt32(&s.maxFly, m, n) {
			break
		}
	}
	s.mu.Lock()
	s.ranges = append(s.ranges, r.Header.Get("Range"))
	hold := s.holdAll
	s.mu.Unlock()

	if lim := atomic.LoadInt32(&s.limit); lim > 0 && n > lim {
		atomic.AddInt32(&s.rejects, 1)
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	if s.slow > 0 && r.Header.Get("Range") != "bytes=0-0" {
		time.Sleep(s.slow)
	}
	if s.delayFor != nil && r.Header.Get("Range") != "bytes=0-0" {
		var start int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
		time.Sleep(s.delayFor(start))
	}

	if !s.noValidator {
		w.Header().Set("ETag", s.etag)
	}
	if s.ignore {
		w.Header().Set("Content-Length", fmt.Sprint(len(s.body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(s.body)
		return
	}
	if hold != nil && r.Header.Get("Range") != "bytes=0-0" {
		// Answer the range request partially, then wait.
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(s.body)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		chunk := int64(4096)
		if end-start+1 < chunk {
			chunk = end - start + 1
		}
		_, _ = w.Write(s.body[start : start+chunk])
		w.(http.Flusher).Flush()
		<-hold
		return
	}
	mod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if s.noValidator {
		mod = time.Time{} // ServeContent then sends no Last-Modified
	}
	http.ServeContent(w, r, "large.bin", mod, bytes.NewReader(s.body))
	s.mu.Lock()
	s.finished = append(s.finished, r.Header.Get("Range"))
	s.mu.Unlock()
}

func (s *segServer) rangeHeaders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ranges...)
}

func (s *segServer) sha() string {
	h := sha256.Sum256(s.body)
	return hex.EncodeToString(h[:])
}

func segItem(s *segServer, name string) site.Item {
	return site.Item{URL: s.srv.URL + "/large.bin", SourcePage: "https://example.test/u/seg", Filename: name, Size: int64(len(s.body))}
}

// --- Tests ---

func TestPlanSegments(t *testing.T) {
	segs := planSegments(100, 3)
	if len(segs) != 3 || segs[0].Start != 0 || segs[0].End != 33 || segs[2].Start != 66 || segs[2].End != 100 {
		t.Fatalf("wrong plan: %+v", segs)
	}
	// NO gaps and NO overlap.
	var covered int64
	for i, sg := range segs {
		covered += sg.End - sg.Start
		if i > 0 && sg.Start != segs[i-1].End {
			t.Fatalf("segments are not contiguous: %+v", segs)
		}
	}
	if covered != 100 {
		t.Fatalf("coverage %d, want 100", covered)
	}
	if got := planSegments(3, 8); len(got) != 3 {
		t.Errorf("more segments than bytes requested: %d", len(got))
	}
	if got := planSegments(0, 4); len(got) != 1 {
		t.Errorf("zero size: %+v", got)
	}
}

// A file downloaded with four segments must be byte-for-byte correct, pass
// sha256, and the server must really see four separate ranges.
func TestSegmentedDownloadIsCorrect(t *testing.T) {
	s := newSegServer(t, 1<<20)
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10}
	it := segItem(s, "large.bin")
	it.SHA256 = s.sha()

	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, s.body) {
		t.Fatal("segmented download content is corrupt")
	}
	if res.SHA256 != s.sha() {
		t.Errorf("sha256 = %s", res.SHA256)
	}
	// 1 probe + 4 segments; the segment ranges must differ.
	ranges := map[string]bool{}
	for _, r := range s.rangeHeaders() {
		if r != "bytes=0-0" {
			ranges[r] = true
		}
	}
	if len(ranges) != 4 {
		t.Errorf("%d distinct ranges requested, want 4: %v", len(ranges), s.rangeHeaders())
	}
	if m := atomic.LoadInt32(&s.maxFly); m < 2 {
		t.Errorf("at most %d concurrent connections; no parallelism", m)
	}
	if _, err := os.Stat(res.Path + ".part.state"); !os.IsNotExist(err) {
		t.Error("state file left behind")
	}
}

// A small file must not be split: a single request (not even a probe).
func TestSmallFileIsNotSegmented(t *testing.T) {
	s := newSegServer(t, 100<<10)
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), Segments: 4} // default threshold 8 MiB
	if _, err := d.Download(context.Background(), out, segItem(s, "small.bin")); err != nil {
		t.Fatal(err)
	}
	if h := atomic.LoadInt32(&s.hits); h != 1 {
		t.Errorf("small file took %d requests, want 1", h)
	}
}

// If the server ignores Range it must fall back to a single stream and the
// file must still arrive correctly.
func TestSegmentedFallsBackWhenRangeIgnored(t *testing.T) {
	s := newSegServer(t, 512<<10)
	s.ignore = true
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10}
	res, err := d.Download(context.Background(), out, segItem(s, "plain.bin"))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, s.body) {
		t.Fatal("the download that fell back to a single stream is corrupt")
	}
}

// With a decoder that can only decode from the start, segmenting must stay
// OFF; one that decodes any range (mega) doesn't stop it.
func TestDecoderDisablesSegments(t *testing.T) {
	d := &Downloader{Segments: 8, Decode: func(site.Item, int64, []byte, io.Reader) (site.DecodedStream, error) { return nil, nil }}
	if n := d.segmentsFor(site.Item{Size: 1 << 30}); n != 1 {
		t.Fatalf("%d segments with a decoder", n)
	}
	d.DecodeRange = func(site.Item, int64, io.Reader) (io.Reader, error) { return nil, nil }
	if n := d.segmentsFor(site.Item{Size: 1 << 30}); n != 8 {
		t.Fatalf("%d segments with a range decoder, want 8", n)
	}
}

// Continuing after an interruption: the segment state must be on disk and the
// second run must only request what is missing (not download from scratch).
func TestSegmentedResumeContinuesPartialSegments(t *testing.T) {
	s := newSegServer(t, 1<<20)
	s.mu.Lock()
	s.holdAll = make(chan struct{})
	s.mu.Unlock()
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10}
	it := segItem(s, "large.bin")
	it.SHA256 = s.sha()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(600 * time.Millisecond); cancel() }()
	_, err := d.Download(ctx, out, it)
	s.mu.Lock()
	close(s.holdAll)
	s.holdAll = nil
	s.mu.Unlock()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}

	statePath := filepath.Join(out, "large.bin.part.state")
	raw, rerr := os.ReadFile(statePath)
	if rerr != nil {
		t.Fatalf("segment state was not written to disk: %v", rerr)
	}
	var st State
	_ = json.Unmarshal(raw, &st)
	if len(st.Segments) != 4 {
		t.Fatalf("%d segments in the state, want 4", len(st.Segments))
	}
	var partial int64
	for _, sg := range st.Segments {
		partial += sg.Done
	}
	if partial == 0 {
		t.Fatal("no segment recorded any progress")
	}
	if fi, _ := os.Stat(filepath.Join(out, "large.bin.part")); fi == nil || fi.Size() != int64(len(s.body)) {
		t.Fatal(".part was not pre-sized")
	}

	before := len(s.rangeHeaders())
	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, s.body) {
		t.Fatal("content corrupted after resume")
	}
	// Resume requests must start where each segment stopped, NOT at the segment start.
	for _, r := range s.rangeHeaders()[before:] {
		var a, b int64
		fmt.Sscanf(r, "bytes=%d-%d", &a, &b)
		for _, sg := range st.Segments {
			if a == sg.Start && sg.Done > 0 {
				t.Errorf("segment %d-%d requested from the start; %d bytes were already downloaded", sg.Start, sg.End, sg.Done)
			}
		}
	}
}

// If the server changes the file while continuing (If-Range rejected -> 200)
// the old segments must be discarded and it must start over; the result must
// still be correct.
func TestSegmentedRestartsWhenSourceChanged(t *testing.T) {
	s := newSegServer(t, 512<<10)
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), Segments: 2, MinSegmentSize: 64 << 10}
	it := segItem(s, "changed.bin")

	// Fake a segmented state left over from an old version: the validator differs.
	part := filepath.Join(out, "changed.bin.part")
	if err := os.WriteFile(part, make([]byte, len(s.body)), 0o644); err != nil {
		t.Fatal(err)
	}
	st := freshState()
	st.TotalSize = int64(len(s.body))
	st.Validator, st.ValidatorType = `"old"`, ValidatorETag
	st.Segments = planSegments(st.TotalSize, 2)
	st.Segments[0].Done = 1000
	saveState(part+".state", st, nil)

	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		// Without a policy the first attempt returns Retryable; the second Download starts clean.
		res, err = d.Download(context.Background(), out, it)
	}
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, s.body) {
		t.Fatal("content stayed corrupt after the source changed")
	}
}

// slotBudget is a host's budget of extra connections whose size a test can
// change while a download runs.
type slotBudget struct {
	mu   sync.Mutex
	cap  int
	used int
	asks int
}

func (b *slotBudget) acquire(_ string, want int) (int, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asks++
	got := b.cap - b.used
	if got > want {
		got = want
	}
	if got < 0 {
		got = 0
	}
	b.used += got
	var once sync.Once
	return got, func() {
		once.Do(func() {
			b.mu.Lock()
			b.used -= got
			b.mu.Unlock()
		})
	}
}

func (b *slotBudget) setCap(n int) {
	b.mu.Lock()
	b.cap = n
	b.mu.Unlock()
}

// Extra connections come out of the host's budget: with one extra slot a
// file gets its own connection plus one, whatever the user asked for.
func TestSegmentsRespectHostSlots(t *testing.T) {
	s := newSegServer(t, 1<<20)
	s.slow = 20 * time.Millisecond
	out := tempDir(t)
	budget := &slotBudget{cap: 1}
	d := &Downloader{
		Client: s.srv.Client(), Segments: 8, MinSegmentSize: 64 << 10, ChunkSize: 64 << 10,
		AcquireExtra: budget.acquire,
	}
	if _, err := d.Download(context.Background(), out, segItem(s, "limited.bin")); err != nil {
		t.Fatal(err)
	}
	if m := atomic.LoadInt32(&s.maxFly); m != 2 {
		t.Errorf("%d concurrent connections; want 2 (its own + 1 extra)", m)
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.used != 0 {
		t.Errorf("%d extra slots were never given back", budget.used)
	}
}

// A slot freed by another download (here: the budget grows) is picked up by
// a download that is already running; it doesn't stay at the number it
// started with.
func TestSegmentedPicksUpFreedSlots(t *testing.T) {
	s := newSegServer(t, 1<<20)
	s.slow = 40 * time.Millisecond
	out := tempDir(t)
	budget := &slotBudget{cap: 0}
	d := &Downloader{
		Client: s.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10, ChunkSize: 32 << 10,
		AcquireExtra: budget.acquire,
	}
	go func() { time.Sleep(150 * time.Millisecond); budget.setCap(3) }()
	if _, err := d.Download(context.Background(), out, segItem(s, "grow.bin")); err != nil {
		t.Fatal(err)
	}
	if m := atomic.LoadInt32(&s.maxFly); m != 4 {
		t.Errorf("at most %d concurrent connections; the freed slots should have brought it to 4", m)
	}
}

// On a sha256 mismatch the .part must be deleted and the error must NOT be retryable.
func TestSegmentedSHA256MismatchRemovesPart(t *testing.T) {
	s := newSegServer(t, 256<<10)
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), Segments: 2, MinSegmentSize: 64 << 10}
	it := segItem(s, "wrong.bin")
	it.SHA256 = "00000000000000000000000000000000ffffffffffffffffffffffffffffffff"
	_, err := d.Download(context.Background(), out, it)
	if !errors.Is(err, ErrSHA256Mismatch) {
		t.Fatalf("expected ErrSHA256Mismatch: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(out, "wrong.bin.part")); !os.IsNotExist(serr) {
		t.Error(".part stayed on disk")
	}
}

// If a .part was left half done by a single stream it must NOT be switched to
// segmented; it must finish as a single stream from where it stopped.
func TestLinearPartIsNotConvertedToSegments(t *testing.T) {
	release := make(chan struct{})
	slow := slowServer(t, 20000, release)
	out := tempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	_, _ = (&Downloader{Client: slow.Client()}).Download(ctx, out, testItem(slow.URL+"/data.bin", "data.bin"))
	close(release)

	srv := rangeServer(t, `"v1"`, nil)
	d := &Downloader{Client: srv.Client(), Segments: 4, MinSegmentSize: 1024}
	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("continuing as a single stream: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(out, "data.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("content is corrupt")
	}
}

// MEASURED (bunkr CDN): out of 4 parallel range requests to the same file one
// gets a 503. One segment's 503 must not bring down the whole download;
// parallelism must drop and the download must still finish correctly.
func TestSegmentedAdaptsToServerConnectionLimit(t *testing.T) {
	s := newSegServer(t, 1<<20)
	atomic.StoreInt32(&s.limit, 2)
	s.slow = 50 * time.Millisecond
	out := tempDir(t)
	// Logf comes from several segment goroutines; an unlocked append would race.
	var logMu sync.Mutex
	var logs []string
	d := &Downloader{
		Client: s.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10,
		Logf: func(f string, a ...any) {
			logMu.Lock()
			logs = append(logs, fmt.Sprintf(f, a...))
			logMu.Unlock()
		},
	}
	it := segItem(s, "limited.bin")
	it.SHA256 = s.sha()

	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatalf("Download: %v (server rejected %d requests)", err, atomic.LoadInt32(&s.rejects))
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, s.body) {
		t.Fatal("content is corrupt")
	}
	if atomic.LoadInt32(&s.rejects) == 0 {
		t.Fatal("test condition did not occur: the server never returned 503")
	}
	shrunk := false
	logMu.Lock()
	defer logMu.Unlock()
	for _, l := range logs {
		if strings.Contains(l, "parallelism") {
			shrunk = true
		}
	}
	if !shrunk {
		t.Errorf("parallelism was not lowered after the 503; logs: %v", logs)
	}
}

// --- Segmented download of a decoded file (mega) ---
//
// A position-keyed XOR stands in for AES-CTR: like CTR, any byte decodes on
// its own given only its offset. The verifier stands in for the meta-MAC: a
// chain over the whole plaintext in order.

func xorKey(pos int64) byte { return byte(pos*131+7) ^ byte(pos>>9) }

func xorAt(b []byte, offset int64) {
	for i := range b {
		b[i] ^= xorKey(offset + int64(i))
	}
}

type xorRangeReader struct {
	r   io.Reader
	pos int64
}

func (x *xorRangeReader) Read(p []byte) (int, error) {
	n, err := x.r.Read(p)
	xorAt(p[:n], x.pos)
	x.pos += int64(n)
	return n, err
}

type shaVerifier struct {
	h    interface{ Sum([]byte) []byte }
	w    io.Writer
	want string
}

func (v *shaVerifier) Write(p []byte) (int, error) { return v.w.Write(p) }
func (v *shaVerifier) Verify() error {
	if got := hex.EncodeToString(v.h.Sum(nil)); got != v.want {
		return fmt.Errorf("fake MAC mismatch: %s", got[:8])
	}
	return nil
}

// newDecodedServer serves the XOR-"encrypted" form of a random plaintext.
func newDecodedServer(t *testing.T, size int) (*segServer, []byte) {
	t.Helper()
	s := newSegServer(t, size)
	plain := append([]byte(nil), s.body...)
	xorAt(s.body, 0) // what goes over the wire
	return s, plain
}

func decodingDownloader(s *segServer, plain []byte, segments int) *Downloader {
	sum := sha256.Sum256(plain)
	want := hex.EncodeToString(sum[:])
	return &Downloader{
		Client: s.srv.Client(), Segments: segments, MinSegmentSize: 64 << 10,
		Decode: func(site.Item, int64, []byte, io.Reader) (site.DecodedStream, error) {
			return nil, errors.New("the single-stream decoder must not be used for a segmented download")
		},
		DecodeRange: func(_ site.Item, offset int64, r io.Reader) (io.Reader, error) {
			return &xorRangeReader{r: r, pos: offset}, nil
		},
		NewVerifier: func(site.Item) (site.Verifier, error) {
			h := sha256.New()
			return &shaVerifier{h: h, w: h, want: want}, nil
		},
	}
}

// The file is fetched over four connections, every segment decodes its own
// range, and PLAINTEXT ends up on disk.
func TestSegmentedDecodedDownload(t *testing.T) {
	s, plain := newDecodedServer(t, 1<<20)
	out := tempDir(t)
	d := decodingDownloader(s, plain, 4)

	res, err := d.Download(context.Background(), out, segItem(s, "enc.bin"))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, plain) {
		if bytes.Equal(got, s.body) {
			t.Fatal("the ENCODED body was written to disk")
		}
		t.Fatal("decoded segmented content is corrupt")
	}
	ranges := map[string]bool{}
	for _, r := range s.rangeHeaders() {
		if r != "bytes=0-0" {
			ranges[r] = true
		}
	}
	if len(ranges) != 4 {
		t.Errorf("%d distinct ranges requested, want 4: %v", len(ranges), s.rangeHeaders())
	}
	want := sha256.Sum256(plain)
	if res.SHA256 != hex.EncodeToString(want[:]) {
		t.Error("the recorded sha256 is not the plaintext's")
	}
}

// A failing integrity check: permanent error, nothing corrupt left on disk.
func TestSegmentedDecodedVerifyFailure(t *testing.T) {
	s, plain := newDecodedServer(t, 512<<10)
	out := tempDir(t)
	wrong := append([]byte(nil), plain...)
	wrong[1000] ^= 1
	d := decodingDownloader(s, wrong, 4) // the verifier expects other content

	_, err := d.Download(context.Background(), out, segItem(s, "bad.bin"))
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity, got %v", err)
	}
	var rt retryableError
	if errors.As(err, &rt) && rt.Retryable() {
		t.Error("an integrity failure must not be retryable")
	}
	for _, name := range []string{"bad.bin", "bad.bin.part", "bad.bin.part.state"} {
		if _, serr := os.Stat(filepath.Join(out, name)); !os.IsNotExist(serr) {
			t.Errorf("%s left on disk after an integrity failure", name)
		}
	}
}

// No ETag and no Last-Modified: If-Range can't protect the segments, but the
// integrity check over the finished file can, so it is still segmented. A
// plain file without either stays a single stream as before.
func TestSegmentedWithoutValidatorNeedsVerifier(t *testing.T) {
	s, plain := newDecodedServer(t, 1<<20)
	s.noValidator = true
	out := tempDir(t)
	d := decodingDownloader(s, plain, 4)
	res, err := d.Download(context.Background(), out, segItem(s, "noval.bin"))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, plain) {
		t.Fatal("content is corrupt")
	}
	if h := atomic.LoadInt32(&s.hits); h != 5 {
		t.Errorf("%d requests, want 5 (1 probe + 4 segments)", h)
	}

	p := newSegServer(t, 1<<20)
	p.noValidator = true
	pd := &Downloader{Client: p.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10}
	if _, err := pd.Download(context.Background(), tempDir(t), segItem(p, "plain.bin")); err != nil {
		t.Fatal(err)
	}
	if h := atomic.LoadInt32(&p.hits); h != 2 {
		t.Errorf("%d requests, want 2 (probe, then a single stream)", h)
	}
}

// The Connections hook reports what is really open: all four segments while
// they run, 0 once they are done.
func TestSegmentedReportsConnections(t *testing.T) {
	s := newSegServer(t, 1<<20)
	s.slow = 150 * time.Millisecond // every segment is in flight before data arrives
	out := tempDir(t)
	var mu sync.Mutex
	var seen []int
	d := &Downloader{
		Client: s.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10,
		Connections: func(_ site.Item, n int) { mu.Lock(); seen = append(seen, n); mu.Unlock() },
	}
	if _, err := d.Download(context.Background(), out, segItem(s, "conns.bin")); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	max := 0
	for _, n := range seen {
		if n > max {
			max = n
		}
	}
	if max != 4 || len(seen) == 0 || seen[len(seen)-1] != 0 {
		t.Errorf("reported connections %v; want a peak of 4 and a final 0", seen)
	}

	// A single stream reports 1.
	var single []int
	sd := &Downloader{Client: s.srv.Client(), Connections: func(_ site.Item, n int) { single = append(single, n) }}
	if _, err := sd.Download(context.Background(), tempDir(t), segItem(s, "one.bin")); err != nil {
		t.Fatal(err)
	}
	if len(single) != 1 || single[0] != 1 {
		t.Errorf("single stream reported %v, want [1]", single)
	}
}

// A download interrupted with four segments, resumed after the user dropped
// to one connection: it continues the segments already on disk (not from
// scratch) and opens only one connection at a time.
func TestSegmentedResumeHonorsFewerConnections(t *testing.T) {
	s := newSegServer(t, 1<<20)
	s.mu.Lock()
	s.holdAll = make(chan struct{})
	s.mu.Unlock()
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10}
	it := segItem(s, "fewer.bin")
	it.SHA256 = s.sha()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(600 * time.Millisecond); cancel() }()
	if _, err := d.Download(ctx, out, it); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	s.mu.Lock()
	close(s.holdAll)
	s.holdAll = nil
	s.mu.Unlock()

	atomic.StoreInt32(&s.maxFly, 0)
	s.slow = 20 * time.Millisecond
	d.SetSegments(1)
	before := len(s.rangeHeaders())
	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, s.body) {
		t.Fatal("content corrupted after resume")
	}
	if m := atomic.LoadInt32(&s.maxFly); m != 1 {
		t.Errorf("resumed with %d concurrent connections, the user asked for 1", m)
	}
	for _, r := range s.rangeHeaders()[before:] {
		if r == "bytes=0-0" || r == "" {
			t.Errorf("the resume started over (request %q); it should continue the segments", r)
		}
	}
}

// --- Chunk queue ---

// The point of the queue: one slow connection holds only its own chunk; the
// other connections take the rest of the file. Before, each connection owned
// a quarter of the file and the end of the file came down over the slowest.
func TestChunkQueueSlowConnectionDoesNotHoldTheRest(t *testing.T) {
	s := newSegServer(t, 1<<20)
	s.delayFor = func(start int64) time.Duration {
		if start == 0 {
			return 700 * time.Millisecond // the first chunk's connection is slow
		}
		return 10 * time.Millisecond
	}
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), Segments: 4, MinSegmentSize: 64 << 10, ChunkSize: 64 << 10}
	it := segItem(s, "queue.bin")
	it.SHA256 = s.sha()

	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, s.body) {
		t.Fatal("content is corrupt")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var chunks []string
	for _, r := range s.finished {
		if r != "bytes=0-0" {
			chunks = append(chunks, r)
		}
	}
	if len(chunks) != 16 {
		t.Fatalf("%d chunk requests, want 16 (1 MiB in 64 KiB chunks): %v", len(chunks), chunks)
	}
	for _, r := range chunks {
		var a, b int64
		fmt.Sscanf(r, "bytes=%d-%d", &a, &b)
		if b-a+1 > 64<<10 {
			t.Errorf("request %q is bigger than a chunk", r)
		}
	}
	// Every other chunk was done by the fast connections while the slow one
	// was still on its first.
	if last := chunks[len(chunks)-1]; !strings.HasPrefix(last, "bytes=0-") {
		t.Errorf("the slow chunk finished before the others (order %v); the rest waited for it", chunks)
	}
}

// Changing "Connections/file" reaches a download that is already running.
func TestChunkQueueFollowsLiveConnectionChoice(t *testing.T) {
	s := newSegServer(t, 1<<20)
	s.slow = 40 * time.Millisecond
	out := tempDir(t)
	d := &Downloader{Client: s.srv.Client(), Segments: 2, MinSegmentSize: 64 << 10, ChunkSize: 32 << 10}
	go func() { time.Sleep(150 * time.Millisecond); d.SetSegments(4) }()
	if _, err := d.Download(context.Background(), out, segItem(s, "live.bin")); err != nil {
		t.Fatal(err)
	}
	if m := atomic.LoadInt32(&s.maxFly); m != 4 {
		t.Errorf("at most %d concurrent connections after raising the choice to 4", m)
	}
}

func TestChunkSizeFor(t *testing.T) {
	const mb = 1 << 20
	cases := []struct {
		size  int64
		want  int
		chunk int64
	}{
		{1024 * mb, 8, 32 * mb},   // big file: capped at 32 MiB
		{200 * mb, 8, 6400 << 10}, // about 4 chunks per connection
		{64 * mb, 4, 4 * mb},      // floor: 4 MiB
		{8 * mb, 8, 1 * mb},       // too small for 4 MiB chunks: one chunk per connection
		{20 * mb, 8, 2621440},     // likewise (20 MiB / 8, rounded up)
	}
	for _, c := range cases {
		if got := chunkSizeFor(c.size, c.want, 0); got != c.chunk {
			t.Errorf("chunkSizeFor(%d MiB, %d) = %d, want %d", c.size/mb, c.want, got, c.chunk)
		}
		segs := planChunks(c.size, chunkSizeFor(c.size, c.want, 0))
		if int64(len(segs)) < int64(c.want) && c.size >= int64(c.want) {
			t.Errorf("%d MiB with %d connections: only %d chunks", c.size/mb, c.want, len(segs))
		}
		var covered int64
		for i, sg := range segs {
			covered += sg.End - sg.Start
			if i > 0 && sg.Start != segs[i-1].End {
				t.Fatalf("chunks are not contiguous: %+v", segs[i-1:i+1])
			}
		}
		if covered != c.size {
			t.Errorf("%d MiB: chunks cover %d bytes", c.size/mb, covered)
		}
	}
	if got := chunkSizeFor(100*mb, 4, 123); got != 123 {
		t.Errorf("a fixed chunk size was not used: %d", got)
	}
}
