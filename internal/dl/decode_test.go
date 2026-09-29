package dl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// --- Fake decoder ---
//
// Position-dependent XOR: every byte is encrypted with a key derived from its
// own position. So the decoder MUST know the offset; if resume starts from
// the wrong place the output is corrupted and the test catches it. State =
// position + plaintext sum; Verify compares the sum with the expected one
// (the meta-MAC's little sibling).

func fakeKey(pos int64) byte { return byte(pos*7 + 3) }

func fakeEncode(plain []byte) []byte {
	out := make([]byte, len(plain))
	for i, b := range plain {
		out[i] = b ^ fakeKey(int64(i))
	}
	return out
}

func fakeSum(plain []byte) uint32 {
	var s uint32
	for _, b := range plain {
		s += uint32(b)
	}
	return s
}

type fakeStream struct {
	r        io.Reader
	pos      int64
	sum      uint32
	wantSum  uint32
	failVery bool
}

func (f *fakeStream) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	for i := 0; i < n; i++ {
		p[i] ^= fakeKey(f.pos)
		f.sum += uint32(p[i])
		f.pos++
	}
	return n, err
}

func (f *fakeStream) State() []byte {
	out := make([]byte, 12)
	binary.BigEndian.PutUint64(out[:8], uint64(f.pos))
	binary.BigEndian.PutUint32(out[8:], f.sum)
	return out
}

func (f *fakeStream) Verify() error {
	if f.failVery {
		return errors.New("deliberate verification failure")
	}
	if f.sum != f.wantSum {
		return fmt.Errorf("sum %d, expected %d", f.sum, f.wantSum)
	}
	return nil
}

type fakeDecoder struct {
	wantSum   uint32
	failVery  bool
	construct []struct {
		offset int64
		saved  int
	}
}

func (d *fakeDecoder) decode(it site.Item, offset int64, saved []byte, r io.Reader) (site.DecodedStream, error) {
	d.construct = append(d.construct, struct {
		offset int64
		saved  int
	}{offset, len(saved)})
	fs := &fakeStream{r: r, wantSum: d.wantSum, failVery: d.failVery}
	if offset > 0 {
		if len(saved) != 12 {
			return nil, errors.New("no decoder state")
		}
		fs.pos = int64(binary.BigEndian.Uint64(saved[:8]))
		fs.sum = binary.BigEndian.Uint32(saved[8:])
		if fs.pos != offset {
			return nil, fmt.Errorf("state says %d, offset %d", fs.pos, offset)
		}
	}
	return fs, nil
}

// encodedRangeServer serves the ENCRYPTED body with Range/If-Range support.
func encodedRangeServer(t *testing.T, etag string) *httptest.Server {
	t.Helper()
	enc := fakeEncode(payload)
	modtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		http.ServeContent(w, r, "data.bin", modtime, bytes.NewReader(enc))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// encodedSlowServer sends the first part of the encrypted body and waits until release.
func encodedSlowServer(t *testing.T, firstChunk int, release <-chan struct{}) *httptest.Server {
	t.Helper()
	enc := fakeEncode(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(enc)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(enc[:firstChunk])
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- Tests ---

// With a decoder attached, PLAINTEXT must be written to disk and the sha256
// must be the plaintext's.
func TestDecodeHookProducesPlaintext(t *testing.T) {
	srv := encodedRangeServer(t, `"v1"`)
	out := tempDir(t)
	dec := &fakeDecoder{wantSum: fakeSum(payload)}
	d := &Downloader{Client: srv.Client(), Decode: dec.decode}

	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA() // hash of the PLAINTEXT
	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, payload) {
		t.Fatal("the content written to disk is not plaintext")
	}
	if res.SHA256 != payloadSHA() {
		t.Errorf("sha256 must be the plaintext's: %s", res.SHA256)
	}
}

// Resume after an interruption: the decoder must be built with the SAVED
// state, from the right offset. Thanks to the position-dependent XOR a wrong
// starting point corrupts the content.
func TestDecodeResumeRestoresDecoderState(t *testing.T) {
	release := make(chan struct{})
	slow := encodedSlowServer(t, 20000, release)
	out := tempDir(t)
	dec := &fakeDecoder{wantSum: fakeSum(payload)}

	ctx, cancel := context.WithCancel(context.Background())
	d := &Downloader{Client: slow.Client(), Decode: dec.decode}
	it := testItem(slow.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	_, err := d.Download(ctx, out, it)
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled: %v", err)
	}

	st := readState(t, filepath.Join(out, "data.bin.part.state"))
	if st.Offset <= 0 {
		t.Fatal("offset did not advance")
	}
	if len(st.DecoderState) == 0 {
		t.Fatal("decoder state was not written to the state; resume is impossible")
	}
	if pos := int64(binary.BigEndian.Uint64(st.DecoderState[:8])); pos != st.Offset {
		t.Fatalf("decoder state says %d, offset %d: they are not taken together", pos, st.Offset)
	}

	srv := encodedRangeServer(t, `"v1"`)
	d2 := &Downloader{Client: srv.Client(), Decode: dec.decode}
	it2 := testItem(srv.URL+"/data.bin", "data.bin")
	it2.SHA256 = payloadSHA()
	if _, err := d2.Download(context.Background(), out, it2); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(out, "data.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("content corrupted after resume: the decoder started from the wrong place")
	}

	last := dec.construct[len(dec.construct)-1]
	if last.offset != st.Offset || last.saved == 0 {
		t.Errorf("on resume the decoder was built with offset=%d saved=%d; expected %d and a non-empty state",
			last.offset, last.saved, st.Offset)
	}
}

// The state must be written to disk WHILE downloading too; if it is only
// written on cancel/error, all progress is lost when the process is killed
// (crash, power cut). It runs with the fake decoder because mega's state
// (decoder + sha256) must be written together with the offset.
func TestStateIsSavedWhileStreaming(t *testing.T) {
	release := make(chan struct{})
	slow := encodedSlowServer(t, 20000, release)
	out := tempDir(t)
	dec := &fakeDecoder{wantSum: fakeSum(payload)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &Downloader{Client: slow.Client(), Decode: dec.decode}
	it := testItem(slow.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()
	errc := make(chan error, 1)
	go func() {
		_, err := d.Download(ctx, out, it)
		errc <- err
	}()

	part := filepath.Join(out, "data.bin.part")
	statePath := part + ".state"
	var st State
	var stateRaw, partRaw []byte
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// Assume the process dies here: whatever is on disk is what stays.
		// The state is read first; since the writer syncs the data before
		// writing the state, the .part always keeps up with the state in
		// this order.
		if raw, err := os.ReadFile(statePath); err == nil {
			var s State
			if json.Unmarshal(raw, &s) == nil && s.Offset > 0 {
				if p, perr := os.ReadFile(part); perr == nil {
					st, stateRaw, partRaw = s, raw, p
					break
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
	cancel()
	<-errc
	if st.Offset <= 0 {
		t.Fatal("the state was not written to disk while downloading; progress is lost if the process dies")
	}
	if int64(len(partRaw)) < st.Offset {
		t.Fatalf(".part is %d bytes, the state says %d: the state is ahead of the data", len(partRaw), st.Offset)
	}
	if pos := int64(binary.BigEndian.Uint64(st.DecoderState[:8])); pos != st.Offset {
		t.Fatalf("decoder state %d, offset %d: not taken together", pos, st.Offset)
	}

	// Continue from the copy taken at crash time: not from scratch, from the saved offset.
	crash := tempDir(t)
	crashPart := filepath.Join(crash, "data.bin.part")
	if err := os.WriteFile(crashPart, partRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crashPart+".state", stateRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := encodedRangeServer(t, `"v1"`)
	d2 := &Downloader{Client: srv.Client(), Decode: dec.decode}
	it2 := testItem(srv.URL+"/data.bin", "data.bin")
	it2.SHA256 = payloadSHA()
	if _, err := d2.Download(context.Background(), crash, it2); err != nil {
		t.Fatalf("continuing after the crash: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(crash, "data.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("continuing after the crash corrupted the content")
	}
	if last := dec.construct[len(dec.construct)-1]; last.offset != st.Offset {
		t.Errorf("continued from offset=%d, expected %d (it started from scratch)", last.offset, st.Offset)
	}
}

// If Verify fails the .part MUST BE DELETED and the error must NOT be
// retryable: if the key is wrong, downloading again produces the same garbage.
func TestDecodeVerifyFailureRemovesPart(t *testing.T) {
	srv := encodedRangeServer(t, `"v1"`)
	out := tempDir(t)
	dec := &fakeDecoder{wantSum: fakeSum(payload), failVery: true}
	d := &Downloader{Client: srv.Client(), Decode: dec.decode}

	_, err := d.Download(context.Background(), out, testItem(srv.URL+"/data.bin", "data.bin"))
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity: %v", err)
	}
	var rt interface{ Retryable() bool }
	if errors.As(err, &rt) && rt.Retryable() {
		t.Error("an integrity error was treated as retryable")
	}
	if _, serr := os.Stat(filepath.Join(out, "data.bin.part")); !os.IsNotExist(serr) {
		t.Error(".part stayed on disk; every run repeats the same failure")
	}
	if _, serr := os.Stat(filepath.Join(out, "data.bin")); !os.IsNotExist(serr) {
		t.Error("unverifiable content was moved to the final name")
	}
}

// With a decoder, resuming from a state without decoder state (from an older
// version or corrupt) is NOT SAFE: it must start from scratch.
func TestDecodeMissingStateForcesRestart(t *testing.T) {
	srv := encodedRangeServer(t, `"v1"`)
	out := tempDir(t)

	// By hand: a half .part + a state WITHOUT decoder state.
	part := filepath.Join(out, "data.bin.part")
	if err := os.WriteFile(part, fakeEncode(payload)[:5000], 0o644); err != nil {
		t.Fatal(err)
	}
	st := freshState()
	st.Offset = 5000
	st.Validator, st.ValidatorType = `"v1"`, ValidatorETag
	st.TotalSize = int64(len(payload))
	saveState(part+".state", st, sha256.New())

	dec := &fakeDecoder{wantSum: fakeSum(payload)}
	d := &Downloader{Client: srv.Client(), Decode: dec.decode}
	it := testItem(srv.URL+"/data.bin", "data.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(dec.construct) == 0 || dec.construct[0].offset != 0 {
		t.Fatalf("the decoder should have been built from scratch: %+v", dec.construct)
	}
	got, _ := os.ReadFile(filepath.Join(out, "data.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("content is corrupt")
	}
}

// WITHOUT a decoder behavior must not change: the decoder field in the state
// must stay empty.
func TestNoDecoderLeavesStateUntouched(t *testing.T) {
	release := make(chan struct{})
	slow := slowServer(t, 20000, release)
	out := tempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	d := &Downloader{Client: slow.Client()}
	_, _ = d.Download(ctx, out, testItem(slow.URL+"/data.bin", "data.bin"))
	close(release)

	st := readState(t, filepath.Join(out, "data.bin.part.state"))
	if len(st.DecoderState) != 0 {
		t.Errorf("decoder_state is set without a decoder: %d bytes", len(st.DecoderState))
	}
}
