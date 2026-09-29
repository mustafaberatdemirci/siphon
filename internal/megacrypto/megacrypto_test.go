package megacrypto

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

// The tests in this file prove that encryption/decryption/MAC are consistent
// WITH THEMSELVES. That the scheme matches mega's real scheme has to be
// verified against a live file; that step is a manual measurement, not part
// of mega_test.go.

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// testMegaKey generates a random key+nonce and packs it with the plaintext's
// meta-MAC: the exact equivalent of the 32 bytes that arrive in a link.
func testMegaKey(t *testing.T, plain []byte) (Key, []byte) {
	t.Helper()
	aesKey := randBytes(t, 16)
	nonce := randBytes(t, 8)
	mac, err := MetaMACOf(aesKey, nonce, plain)
	if err != nil {
		t.Fatal(err)
	}
	full := PackFileKey(aesKey, nonce, mac)
	k, err := UnpackFileKey(full)
	if err != nil {
		t.Fatal(err)
	}
	return k, full
}

func TestMegaKeyPackUnpackRoundTrip(t *testing.T) {
	aesKey, nonce, mac := randBytes(t, 16), randBytes(t, 8), randBytes(t, 8)
	k, err := UnpackFileKey(PackFileKey(aesKey, nonce, mac))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k.AES, aesKey) || !bytes.Equal(k.Nonce, nonce) || !bytes.Equal(k.MAC, mac) {
		t.Fatal("pack/unpack corrupted the key")
	}
	if _, err := UnpackFileKey(make([]byte, 16)); err == nil {
		t.Error("a 16-byte file key was accepted")
	}
}

func TestMegaB64Variants(t *testing.T) {
	want := []byte{0xfb, 0xff, 0xbf, 0xfa}
	for _, in := range []string{"-_-_-g", "+/+/+g", "-_-_-g==", "-_-_-g,,"} {
		got, err := B64Decode(in)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("B64Decode(%q) = %x, %v", in, got, err)
		}
	}
}

func TestMegaAttrsRoundTripAndWrongKey(t *testing.T) {
	key := randBytes(t, 16)
	const name = "Holiday Video — Zoë & Chloé.mp4"
	at, err := EncryptAttrs(key, Attrs{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	a, err := DecryptAttrs(key, at)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != name {
		t.Errorf("name = %q", a.Name)
	}
	// A wrong key does not produce the "MEGA" prefix: the error must be clear,
	// not a garbage name.
	if _, err := DecryptAttrs(randBytes(t, 16), at); err == nil {
		t.Error("attributes were 'decrypted' with a wrong key")
	}
}

func TestMegaNodeKeyRoundTrip(t *testing.T) {
	folderKey := randBytes(t, 16)
	nodeKey := randBytes(t, 32)
	enc, err := EncryptNodeKey(folderKey, nodeKey)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := DecryptNodeKey(folderKey, enc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec, nodeKey) {
		t.Fatal("node key was corrupted")
	}
}

// Chunk boundaries follow mega's layout: 128K, 384K, 768K, 1280K, 1920K,
// 2688K, 3584K, 4608K, then every 1M.
func TestMegaChunkBoundaries(t *testing.T) {
	const k = 1024
	cases := []struct{ pos, end int64 }{
		{0, 128 * k},
		{128*k - 1, 128 * k},
		{128 * k, 384 * k},
		{384 * k, 768 * k},
		{768 * k, 1280 * k},
		{4608*k - 1, 4608 * k},
		{4608 * k, 5632 * k},
		{5632 * k, 6656 * k},
		// After 4.5 MiB the boundaries are NOT aligned to whole MiB: 4608K + n*1024K.
		// 100 MiB = 102400K; the chunk containing it ends at 102912K (100.5 MiB).
		{100 * 1024 * k, 102912 * k},
	}
	for _, c := range cases {
		if got := ChunkEnd(c.pos); got != c.end {
			t.Errorf("ChunkEnd(%d) = %d, want %d", c.pos, got, c.end)
		}
	}
}

// Full stream: encrypt -> decrypt -> verify. The size is chosen to exercise
// chunk boundaries and partial blocks (several chunks, not a multiple of 16).
func TestMegaStreamRoundTrip(t *testing.T) {
	plain := randBytes(t, 300*1024+7)
	key, _ := testMegaKey(t, plain)
	enc, err := EncryptCTR(key.AES, key.Nonce, plain)
	if err != nil {
		t.Fatal(err)
	}

	ms, err := NewStream(key, 0, nil, bytes.NewReader(enc))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(ms)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("decrypted content differs from the plaintext")
	}
	if err := ms.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// If the file ends exactly on a chunk boundary, no empty chunk may be folded in.
func TestMegaStreamExactChunkBoundary(t *testing.T) {
	plain := randBytes(t, 128*1024)
	key, _ := testMegaKey(t, plain)
	enc, _ := EncryptCTR(key.AES, key.Nonce, plain)
	ms, _ := NewStream(key, 0, nil, bytes.NewReader(enc))
	if _, err := io.ReadAll(ms); err != nil {
		t.Fatal(err)
	}
	if err := ms.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// If a single byte changes, verification MUST fail. This is the only check
// that keeps a corrupt download from counting as "successful": mega does not
// provide sha256.
func TestMegaStreamDetectsTampering(t *testing.T) {
	plain := randBytes(t, 200*1024)
	key, _ := testMegaKey(t, plain)
	enc, _ := EncryptCTR(key.AES, key.Nonce, plain)
	enc[150*1024] ^= 0x01

	ms, _ := NewStream(key, 0, nil, bytes.NewReader(enc))
	if _, err := io.ReadAll(ms); err != nil {
		t.Fatal(err)
	}
	if err := ms.Verify(); err == nil {
		t.Fatal("corrupted content passed verification")
	}
}

// Resume: when the stream is cut at an arbitrary point and continued with the
// saved state, both the content and the MAC must be correct. The cut points
// are deliberately awkward: mid-block, mid-chunk, exactly on a chunk
// boundary, right after the first byte.
func TestMegaStreamResumeAtAwkwardOffsets(t *testing.T) {
	plain := randBytes(t, 400*1024+5)
	key, _ := testMegaKey(t, plain)
	enc, _ := EncryptCTR(key.AES, key.Nonce, plain)

	for _, cut := range []int{1, 15, 16, 17, 1000, 128 * 1024, 128*1024 + 3, 383 * 1024, 384 * 1024, len(enc) - 1} {
		first, err := NewStream(key, 0, nil, bytes.NewReader(enc[:cut]))
		if err != nil {
			t.Fatal(err)
		}
		head, _ := io.ReadAll(first)
		saved := first.State()

		second, err := NewStream(key, int64(cut), saved, bytes.NewReader(enc[cut:]))
		if err != nil {
			t.Fatalf("cut=%d: could not set up resume: %v", cut, err)
		}
		tail, _ := io.ReadAll(second)

		if got := append(head, tail...); !bytes.Equal(got, plain) {
			t.Fatalf("cut=%d: content corrupted after resume", cut)
		}
		if err := second.Verify(); err != nil {
			t.Fatalf("cut=%d: Verify after resume: %v", cut, err)
		}
	}
}

// If the state and the offset disagree the decoder must NOT be built;
// silently starting from the wrong place means verification fails at the very
// end, i.e. the whole download is wasted.
func TestMegaStreamRejectsMismatchedState(t *testing.T) {
	plain := randBytes(t, 50*1024)
	key, _ := testMegaKey(t, plain)
	enc, _ := EncryptCTR(key.AES, key.Nonce, plain)

	first, _ := NewStream(key, 0, nil, bytes.NewReader(enc[:1000]))
	_, _ = io.ReadAll(first)
	saved := first.State()

	if _, err := NewStream(key, 2000, saved, bytes.NewReader(enc[2000:])); err == nil {
		t.Error("resume from 2000 accepted while the state says 1000")
	}
	if _, err := NewStream(key, 1000, nil, bytes.NewReader(enc[1000:])); err == nil {
		t.Error("resume without state accepted")
	}
	if _, err := NewStream(key, 1000, saved[:10], bytes.NewReader(enc[1000:])); err == nil {
		t.Error("truncated state accepted")
	}
}

// State() must reflect the number of bytes read so far on every call: the
// downloader takes it together with the offset.
func TestMegaStreamStateTracksPosition(t *testing.T) {
	plain := randBytes(t, 10*1024)
	key, _ := testMegaKey(t, plain)
	enc, _ := EncryptCTR(key.AES, key.Nonce, plain)
	ms, _ := NewStream(key, 0, nil, bytes.NewReader(enc))

	buf := make([]byte, 777)
	var read int64
	for {
		n, err := ms.Read(buf)
		read += int64(n)
		probe := &Stream{}
		if rerr := probe.restore(ms.State()); rerr != nil {
			t.Fatal(rerr)
		}
		if probe.pos != read {
			t.Fatalf("State pos=%d, read=%d", probe.pos, read)
		}
		if err == io.EOF {
			break
		}
	}
}

// A file fetched over several connections: every range must decrypt on its
// own to exactly the bytes of the same range of the full plaintext, at the
// awkward offsets too (mid-block, block boundary, chunk boundary, the end).
func TestRangeReaderDecryptsAnyRange(t *testing.T) {
	plain := randBytes(t, 3<<20+77)
	k, _ := testMegaKey(t, plain)
	enc, err := EncryptCTR(k.AES, k.Nonce, plain)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range [][2]int64{
		{0, 1}, {0, 16}, {5, 21}, {16, 32}, {15, 17},
		{128 << 10, 384 << 10},       // exactly the first two chunk boundaries
		{(128 << 10) - 3, 1<<20 + 5}, // straddles several chunks
		{int64(len(plain)) - 9, int64(len(plain))},
	} {
		rd, err := NewRangeReader(k, r[0], bytes.NewReader(enc[r[0]:r[1]]))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rd)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, plain[r[0]:r[1]]) {
			t.Errorf("range %d-%d decrypted wrong", r[0], r[1])
		}
	}
	if _, err := NewRangeReader(k, -1, bytes.NewReader(nil)); err == nil {
		t.Error("a negative offset was accepted")
	}
}

// The verifier must accept the right plaintext however it is written in
// (one call or odd-sized pieces) and reject a single flipped bit.
func TestVerifierOverFinishedPlaintext(t *testing.T) {
	for _, size := range []int{0, 1, 128 << 10, 128<<10 + 1, 5<<20 + 13} {
		plain := randBytes(t, size)
		k, _ := testMegaKey(t, plain)

		v, err := NewVerifier(k)
		if err != nil {
			t.Fatal(err)
		}
		for p := plain; len(p) > 0; {
			n := 7919 // prime: pieces never line up with blocks or chunks
			if n > len(p) {
				n = len(p)
			}
			_, _ = v.Write(p[:n])
			p = p[n:]
		}
		if err := v.Verify(); err != nil {
			t.Errorf("size %d: the right plaintext was rejected: %v", size, err)
		}

		if size == 0 {
			continue
		}
		bad := append([]byte(nil), plain...)
		bad[size/2] ^= 0x01
		v2, _ := NewVerifier(k)
		_, _ = v2.Write(bad)
		if err := v2.Verify(); err == nil {
			t.Errorf("size %d: a corrupted plaintext passed", size)
		}
	}
}
