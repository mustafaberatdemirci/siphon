package run

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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/dl"
	"github.com/mustafaberatdemirci/siphon/internal/megacrypto"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/store"
)

// End to end: the real mega resolver + the real downloader + the real
// ledger, with a single httptest server playing both the API and storage. The
// encrypted body arrives over the wire, PLAINTEXT is written to disk and the
// meta-MAC is verified.
//
// The unit tests in mega_test.go prove the resolver on its own; this test
// proves the pieces work TOGETHER: that Item.Secret reaches the downloader,
// that the decoder is built with the right offset, that on resume the decoder
// state comes back from the state file.

type megaE2E struct {
	t      *testing.T
	srv    *httptest.Server
	plain  []byte
	packed []byte
	enc    []byte
	name   string

	mu        sync.Mutex
	hold      chan struct{} // if non-nil the FIRST body waits on this channel
	holdUsed  bool          // the channel reference stays in the test; whether it was consumed is tracked separately
	firstOnly int           // bytes to send in the first response while holding
	dlHits    int
	// dlPaths are the storage requests' paths in order; rangeHeaders counts
	// the ones that carried a Range header (the real servers aren't known
	// to honor it).
	dlPaths      []string
	rangeHeaders int
}

func newMegaE2E(t *testing.T, size int) *megaE2E {
	t.Helper()
	e := &megaE2E{t: t, name: "Private Video — Zoë.mp4"}
	e.plain = make([]byte, size)
	if _, err := rand.Read(e.plain); err != nil {
		t.Fatal(err)
	}
	aesKey, nonce := make([]byte, 16), make([]byte, 8)
	_, _ = rand.Read(aesKey)
	_, _ = rand.Read(nonce)
	mac, err := megacrypto.MetaMACOf(aesKey, nonce, e.plain)
	if err != nil {
		t.Fatal(err)
	}
	e.packed = megacrypto.PackFileKey(aesKey, nonce, mac)
	e.enc, err = megacrypto.EncryptCTR(aesKey, nonce, e.plain)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/cs", e.api)
	mux.HandleFunc("/dl/", e.download)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *megaE2E) api(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var cmds []map[string]any
	if err := json.Unmarshal(body, &cmds); err != nil || len(cmds) == 0 {
		fmt.Fprint(w, "-2")
		return
	}
	if cmds[0]["a"] != "g" {
		fmt.Fprint(w, "[-2]")
		return
	}
	key, _ := megacrypto.UnpackFileKey(e.packed)
	at, _ := megacrypto.EncryptAttrs(key.AES, megacrypto.Attrs{Name: e.name})
	_ = json.NewEncoder(w).Encode([]any{map[string]any{
		"s": len(e.plain), "at": at, "g": e.srv.URL + "/dl/FiLeHaNd",
	}})
}

// download plays mega's storage server: the byte range is in the path
// ("/dl/<handle>/<start>-<end>", end inclusive) and the answer is 200 with
// exactly that range. A Range header is ignored (counted, so a test can tell
// the downloader relied on it), and there is no ETag or Last-Modified: it is
// not known whether the real servers send one.
func (e *megaE2E) download(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.dlHits++
	e.dlPaths = append(e.dlPaths, r.URL.Path)
	if r.Header.Get("Range") != "" {
		e.rangeHeaders++
	}
	var hold chan struct{}
	if e.hold != nil && !e.holdUsed {
		hold, e.holdUsed = e.hold, true
	}
	first := e.firstOnly
	e.mu.Unlock()

	start, end := int64(0), int64(len(e.enc))
	if rest := strings.TrimPrefix(r.URL.Path, "/dl/FiLeHaNd/"); rest != r.URL.Path {
		a, b, dash := strings.Cut(rest, "-")
		start, _ = strconv.ParseInt(a, 10, 64)
		if dash && b != "" {
			last, _ := strconv.ParseInt(b, 10, 64)
			end = last + 1
		}
	}
	if start < 0 || end > int64(len(e.enc)) || start >= end {
		http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprint(end-start))
	w.WriteHeader(http.StatusOK)
	if hold != nil {
		// First body: send the chunk, then wait forever (simulated interruption).
		_, _ = w.Write(e.enc[start : start+int64(first)])
		w.(http.Flusher).Flush()
		<-hold
		return
	}
	_, _ = w.Write(e.enc[start:end])
}

func (e *megaE2E) link() string {
	return "https://mega.nz/file/FiLeHaNd#" + megacrypto.B64Encode(e.packed)
}

func (e *megaE2E) resolver() site.Resolver {
	cfg := site.SiteConfig{
		Name:          site.MegaName,
		Domains:       []string{"mega.nz"},
		RefererPolicy: site.RefererNone,
		Extra:         map[string]string{site.ExtraMegaAPI: e.srv.URL + "/cs"},
		HTTPClient:    e.srv.Client(),
	}.WithDefaults()
	return site.NewMega(cfg)
}

func (e *megaE2E) runCtx(t *testing.T, outDir string, ev Events, ctx context.Context) runCtx {
	t.Helper()
	ledger, err := store.Open(outDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	r := e.resolver()
	cfg := site.SiteConfig{Name: site.MegaName}.WithDefaults()
	cfg.MaxRetries = 2
	return runCtx{
		url: e.link(), resolver: r, cfg: cfg, client: e.srv.Client(),
		ledger: ledger, opt: Options{OutDir: outDir}, ev: ev, inFlight: 2,
	}
}

func TestMegaEndToEndDownloadsPlaintext(t *testing.T) {
	e := newMegaE2E(t, 300*1024+11)
	out := tempDir(t)

	var done []dl.Result
	var failed []error
	ev := Events{
		ItemDone:   func(_ site.Item, r dl.Result) { done = append(done, r) },
		ItemFailed: func(_ site.Item, err error) { failed = append(failed, err) },
	}
	res := runOne(context.Background(), e.runCtx(t, out, ev, context.Background()))
	if res.resolveErr != nil {
		t.Fatalf("resolution: %v", res.resolveErr)
	}
	if len(failed) != 0 {
		t.Fatalf("failed: %v", failed)
	}
	if len(done) != 1 {
		t.Fatalf("%d files downloaded, want 1", len(done))
	}

	// What was written to disk must be PLAINTEXT, not the encrypted body from the wire.
	got, err := os.ReadFile(done[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, e.plain) {
		if bytes.Equal(got, e.enc) {
			t.Fatal("the ENCRYPTED body was written to disk: the decoder didn't kick in")
		}
		t.Fatal("content is neither plaintext nor encrypted: corrupt")
	}
	if filepath.Base(done[0].Path) != e.name {
		t.Errorf("file name = %q, expected the name from the attributes", filepath.Base(done[0].Path))
	}
	want := sha256.Sum256(e.plain)
	if done[0].SHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("the recorded sha256 is not the plaintext's")
	}

	// The ledger stores the source link as is, and for mega it CARRIES the
	// link key (after the #). This is deliberate: re-resolution needs that
	// key, and the link is something the user already has. Here we only
	// verify that the entry was written and points at the file.
	ledgerBytes, _ := os.ReadFile(filepath.Join(out, store.FileName))
	if !strings.Contains(string(ledgerBytes), e.name) {
		t.Error("the ledger file doesn't contain the downloaded file")
	}
}

// Interruption + resume through the REAL downloader: the decoder state must
// be written to the state file and the second run must continue from there
// and pass the meta-MAC.
func TestMegaEndToEndResume(t *testing.T) {
	e := newMegaE2E(t, 400*1024+3)
	out := tempDir(t)

	// Run 1: a server that sends 150 KB and locks up, interrupted by context cancellation.
	e.hold = make(chan struct{})
	e.firstOnly = 150 * 1024
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	rc := e.runCtx(t, out, Events{}, ctx)
	_ = runOne(ctx, rc)
	close(e.hold) // release the held handler; the server waits for it in Cleanup

	part := filepath.Join(out, e.name+".part")
	statePath := part + ".state"
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("no state, the interruption was not recorded: %v", err)
	}
	var st dl.State
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Offset <= 0 || st.Offset >= int64(len(e.plain)) {
		t.Fatalf("offset = %d, partial progress expected", st.Offset)
	}
	if len(st.DecoderState) == 0 {
		t.Fatal("no decoder state in the state; resume would compute the MAC wrong")
	}

	// Run 2: a normal server, from where it left off.
	var done []dl.Result
	var failed []error
	ev := Events{
		ItemDone:   func(_ site.Item, r dl.Result) { done = append(done, r) },
		ItemFailed: func(_ site.Item, err error) { failed = append(failed, err) },
	}
	res := runOne(context.Background(), e.runCtx(t, out, ev, context.Background()))
	if res.resolveErr != nil || len(failed) != 0 {
		t.Fatalf("resume run: %v / %v", res.resolveErr, failed)
	}
	if len(done) != 1 {
		t.Fatalf("%d files downloaded, want 1", len(done))
	}
	got, _ := os.ReadFile(done[0].Path)
	if !bytes.Equal(got, e.plain) {
		t.Fatal("content corrupted after resume")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Error("the completed file's state was not deleted")
	}
	// The second run must have continued with a Range, not downloaded from scratch.
	e.mu.Lock()
	hits := e.dlHits
	e.mu.Unlock()
	if hits != 2 {
		t.Errorf("storage called %d times, want 2 (1 cut + 1 resume)", hits)
	}
	e.mu.Lock()
	paths, withRange := append([]string(nil), e.dlPaths...), e.rangeHeaders
	e.mu.Unlock()
	if len(paths) == 2 && (!strings.HasPrefix(paths[1], "/dl/FiLeHaNd/") || strings.HasPrefix(paths[1], "/dl/FiLeHaNd/0-")) {
		t.Errorf("the resume asked for %q; want the rest in the path, from where it stopped", paths[1])
	}
	if withRange != 0 {
		t.Errorf("%d storage requests carried a Range header", withRange)
	}
}

// Third run: the file is already in the ledger -> must be skipped without even going to the API.
func TestMegaEndToEndSecondRunSkips(t *testing.T) {
	e := newMegaE2E(t, 64*1024)
	out := tempDir(t)

	first := runOne(context.Background(), e.runCtx(t, out, Events{}, context.Background()))
	if first.done != 1 {
		t.Fatalf("first run: %+v", first)
	}
	var skipped int
	ev := Events{ItemSkipped: func(site.Item, store.Entry) { skipped++ }}
	second := runOne(context.Background(), e.runCtx(t, out, ev, context.Background()))
	if second.skipped != 1 || skipped != 1 {
		t.Fatalf("the second run didn't skip: %+v", second)
	}
}

// The real mega resolver, the real crypto and the real downloader over
// several connections: every range is decrypted on its own (at offsets that
// cut through AES blocks and MAC chunks), the meta-MAC is checked over the
// finished file. Before, a mega file was one connection whatever the user chose.
func TestMegaEndToEndSegmented(t *testing.T) {
	e := newMegaE2E(t, 2<<20+333)
	out := tempDir(t)
	r := e.resolver()
	cfg := site.SiteConfig{Name: site.MegaName, MaxSegments: 4, MaxConcurrent: 8, MaxRetries: 1}.WithDefaults()
	var mu sync.Mutex
	maxConns := 0
	w := NewWorker(r, cfg, e.srv.Client(), Events{Connections: func(_ site.Item, n int) {
		mu.Lock()
		if n > maxConns {
			maxConns = n
		}
		mu.Unlock()
	}})
	w.Down.MinSegmentSize = 64 << 10

	var it site.Item
	if _, err := r.Resolve(context.Background(), e.link(), func(x site.Item) error { it = x; return nil }); err != nil {
		t.Fatal(err)
	}
	outc := w.DownloadItem(context.Background(), out, nil, it, Events{})
	if outc.Kind != OutcomeDone {
		t.Fatalf("outcome %v: %v", outc.Kind, outc.Err)
	}
	got, err := os.ReadFile(outc.Result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, e.plain) {
		if bytes.Equal(got, e.enc) {
			t.Fatal("the ENCRYPTED body was written to disk")
		}
		t.Fatal("segmented mega content is corrupt")
	}
	e.mu.Lock()
	hits := e.dlHits
	e.mu.Unlock()
	if hits != 5 {
		t.Errorf("storage called %d times, want 5 (1 probe + 4 segments)", hits)
	}
	e.mu.Lock()
	if e.rangeHeaders != 0 {
		t.Errorf("%d storage requests carried a Range header; mega takes the range in the path", e.rangeHeaders)
	}
	e.mu.Unlock()
	mu.Lock()
	if maxConns != 4 {
		t.Errorf("at most %d connections reported, want 4", maxConns)
	}
	mu.Unlock()
}

// A wrong key MAC must fail the segmented download too, and leave nothing
// corrupt behind.
func TestMegaEndToEndSegmentedDetectsCorruption(t *testing.T) {
	e := newMegaE2E(t, 1<<20)
	e.enc[512<<10] ^= 0xff // one byte flipped on the wire
	out := tempDir(t)
	r := e.resolver()
	cfg := site.SiteConfig{Name: site.MegaName, MaxSegments: 4, MaxConcurrent: 8, MaxRetries: 1}.WithDefaults()
	w := NewWorker(r, cfg, e.srv.Client(), Events{})
	w.Down.MinSegmentSize = 64 << 10

	var it site.Item
	if _, err := r.Resolve(context.Background(), e.link(), func(x site.Item) error { it = x; return nil }); err != nil {
		t.Fatal(err)
	}
	outc := w.DownloadItem(context.Background(), out, nil, it, Events{})
	if outc.Kind != OutcomeFailed || !errors.Is(outc.Err, dl.ErrIntegrity) {
		t.Fatalf("outcome %v (%v), want a failed integrity check", outc.Kind, outc.Err)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		names := []string{}
		for _, en := range entries {
			names = append(names, en.Name())
		}
		t.Errorf("left on disk after the integrity failure: %v", names)
	}
}
