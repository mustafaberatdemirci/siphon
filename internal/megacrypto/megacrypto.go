// Package megacrypto implements mega.nz's client-side encryption scheme.
// It is standard library only; it is a separate package because it is
// independent of the site protocol: both the resolver and the tests that
// build a fake server use it.
package megacrypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// mega's client-side encryption. Everything is standard library; the only
// mega-specific parts are how the key is packed, how the attributes are
// wrapped and the chunk boundaries of the integrity MAC.
//
// Sources: mega's own web client and independent implementations (mega.py,
// megatools, plowshare) describe the same scheme:
//   - content: AES-128-CTR, IV = nonce(8) || block counter(8, big-endian)
//   - attributes: AES-128-CBC, zero IV, "MEGA" prefix + JSON + zero padding
//   - integrity: per-chunk CBC-MAC (IV = nonce||nonce), chunk MACs are folded
//     again with CBC-MAC, the 16-byte file MAC is XORed down to 8 bytes
//
// The tests in this package prove internal consistency; that the scheme is
// byte-for-byte identical to mega's has to be verified against a LIVE file.
// We did the same for the XOR scheme: a byte-by-byte comparison against an
// independent implementation.

// B64Decode decodes mega's URL-safe, unpadded base64. Old links may use ','
// instead of '='; the standard alphabet is accepted too.
func B64Decode(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer("+", "-", "/", "_", "=", "", ",", "").Replace(s)
	return base64.RawURLEncoding.DecodeString(s)
}

func B64Encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Key is the material derived from the 32-byte file key.
type Key struct {
	AES   []byte // 16: content and attribute key
	Nonce []byte // 8: upper half of the CTR counter and the MAC IV
	MAC   []byte // 8: expected meta-MAC
}

// UnpackFileKey unpacks the 32 bytes from the link: XOR of the first 16 and
// the last 16 gives the AES key; the last 16 themselves are nonce(8) +
// meta-MAC(8).
func UnpackFileKey(full []byte) (Key, error) {
	if len(full) != 32 {
		return Key{}, fmt.Errorf("file key is %d bytes, expected 32", len(full))
	}
	k := Key{AES: make([]byte, 16), Nonce: make([]byte, 8), MAC: make([]byte, 8)}
	for i := 0; i < 16; i++ {
		k.AES[i] = full[i] ^ full[16+i]
	}
	copy(k.Nonce, full[16:24])
	copy(k.MAC, full[24:32])
	return k, nil
}

// PackFileKey is the inverse of unpack. Used in tests and when putting a
// folder node's key into Item.Secret.
func PackFileKey(aesKey, nonce, mac []byte) []byte {
	full := make([]byte, 32)
	copy(full[16:24], nonce)
	copy(full[24:32], mac)
	for i := 0; i < 16; i++ {
		full[i] = aesKey[i] ^ full[16+i]
	}
	return full
}

// Attrs is the content of the encrypted attribute block. Only the name is
// needed for now.
type Attrs struct {
	Name string `json:"n"`
}

// DecryptAttrs decrypts the "at" field. A block decrypted with the wrong key
// does not start with "MEGA"; that is the cheap way to test a key without
// downloading the content.
func DecryptAttrs(aesKey []byte, at string) (Attrs, error) {
	raw, err := B64Decode(at)
	if err != nil {
		return Attrs{}, fmt.Errorf("attributes are not base64: %w", err)
	}
	if len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		return Attrs{}, fmt.Errorf("attribute length %d is not a multiple of 16", len(raw))
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return Attrs{}, err
	}
	cipher.NewCBCDecrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(raw, raw)
	if !bytes.HasPrefix(raw, []byte("MEGA")) {
		return Attrs{}, errors.New("could not decrypt attributes: the key may be wrong")
	}
	js := bytes.TrimRight(raw[4:], "\x00")
	var a Attrs
	if err := json.Unmarshal(js, &a); err != nil {
		return Attrs{}, fmt.Errorf("attributes are not JSON: %w", err)
	}
	return a, nil
}

// EncryptAttrs is the inverse of DecryptAttrs; for tests and the fake server.
func EncryptAttrs(aesKey []byte, a Attrs) (string, error) {
	js, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	plain := append([]byte("MEGA"), js...)
	if pad := len(plain) % aes.BlockSize; pad != 0 {
		plain = append(plain, make([]byte, aes.BlockSize-pad)...)
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", err
	}
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(out, plain)
	return B64Encode(out), nil
}

// DecryptNodeKey unpacks the key of a node in a folder link: the node key
// arrives encrypted with the folder key using AES-ECB (block by block).
func DecryptNodeKey(folderKey, enc []byte) ([]byte, error) {
	if len(enc) == 0 || len(enc)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("node key is %d bytes, not a multiple of 16", len(enc))
	}
	block, err := aes.NewCipher(folderKey)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(enc))
	for i := 0; i < len(enc); i += aes.BlockSize {
		block.Decrypt(out[i:i+aes.BlockSize], enc[i:i+aes.BlockSize])
	}
	return out, nil
}

// EncryptNodeKey is the inverse of DecryptNodeKey; for tests.
func EncryptNodeKey(folderKey, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(folderKey)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(plain))
	for i := 0; i < len(plain); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], plain[i:i+aes.BlockSize])
	}
	return out, nil
}

// ChunkEnd returns the end (exclusive) of the MAC chunk containing pos.
//
// mega's chunk layout: the first eight chunks are 128K, 256K, ..., 1M (each
// 128K larger than the previous), after that always 1M. Boundaries derive
// from the absolute position, so they can be rebuilt on resume.
func ChunkEnd(pos int64) int64 {
	const step = 128 << 10
	var b int64
	for i := int64(1); i <= 8; i++ {
		b += i * step
		if pos < b {
			return b
		}
	}
	const big = 1 << 20
	return b + ((pos-b)/big+1)*big
}

// Stream is a reader that decrypts the encrypted body and runs the integrity
// MAC. It offers the same three methods as site.DecodedStream (Read, State,
// Verify).
type Stream struct {
	r     io.Reader
	block cipher.Block
	ctr   cipher.Stream
	key   Key

	pos        int64    // plaintext position from the start of the file
	fileMAC    [16]byte // folded MAC of finished chunks
	chunkMAC   [16]byte // CBC-MAC state of the open chunk
	chunkStart int64    // start of the open chunk
	partial    [16]byte // incomplete last block of the open chunk
	partialN   int
}

const stateVersion = 1

// stateLen: version(1) + pos(8) + fileMAC(16) + chunkMAC(16) + chunkStart(8)
// + partialN(1) + partial(16).
const stateLen = 1 + 8 + 16 + 16 + 8 + 1 + 16

func NewStream(key Key, offset int64, saved []byte, r io.Reader) (*Stream, error) {
	block, err := aes.NewCipher(key.AES)
	if err != nil {
		return nil, err
	}
	m := &Stream{r: r, block: block, key: key}

	if offset == 0 {
		m.resetChunk(0)
	} else {
		// If we start anywhere but zero the MAC state MUST have been saved;
		// otherwise verification is wrong from the start and we'd only learn
		// that when the file ends, after the whole download was wasted.
		if err := m.restore(saved); err != nil {
			return nil, fmt.Errorf("mega decoder state: %w", err)
		}
		if m.pos != offset {
			return nil, fmt.Errorf("mega decoder state says %d bytes, resume requested from %d", m.pos, offset)
		}
	}

	m.ctr = ctrAt(block, key.Nonce, offset)
	return m, nil
}

// ctrAt is the AES-CTR key stream positioned at a plaintext offset. The
// counter is in 16-byte blocks; the remainder within the block is skipped.
// CTR needs no earlier state, which is what lets any byte range be decrypted
// on its own.
func ctrAt(block cipher.Block, nonce []byte, offset int64) cipher.Stream {
	iv := make([]byte, aes.BlockSize)
	copy(iv, nonce)
	binary.BigEndian.PutUint64(iv[8:], uint64(offset/aes.BlockSize))
	s := cipher.NewCTR(block, iv)
	if skip := int(offset % aes.BlockSize); skip > 0 {
		scratch := make([]byte, skip)
		s.XORKeyStream(scratch, scratch)
	}
	return s
}

// NewRangeReader decrypts a body whose first byte sits at plaintext offset
// offset. Unlike Stream it keeps no MAC state: the integrity check of a file
// fetched in ranges runs once over the finished plaintext (NewVerifier).
func NewRangeReader(key Key, offset int64, r io.Reader) (io.Reader, error) {
	if offset < 0 {
		return nil, fmt.Errorf("negative offset %d", offset)
	}
	block, err := aes.NewCipher(key.AES)
	if err != nil {
		return nil, err
	}
	return &rangeReader{r: r, ctr: ctrAt(block, key.Nonce, offset)}, nil
}

type rangeReader struct {
	r   io.Reader
	ctr cipher.Stream
}

func (rr *rangeReader) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	if n > 0 {
		rr.ctr.XORKeyStream(p[:n], p[:n])
	}
	return n, err
}

// Verifier computes the meta-MAC over plaintext written to it in order, from
// the first byte to the last. A file fetched over several connections has no
// single stream to carry the MAC; the finished file is read once instead
// (the downloader hashes it for sha256 in the same pass).
type Verifier struct{ s Stream }

func NewVerifier(key Key) (*Verifier, error) {
	block, err := aes.NewCipher(key.AES)
	if err != nil {
		return nil, err
	}
	v := &Verifier{s: Stream{block: block, key: key}}
	v.s.resetChunk(0)
	return v, nil
}

func (v *Verifier) Write(p []byte) (int, error) {
	v.s.macWrite(p)
	return len(p), nil
}

// Verify compares the meta-MAC of everything written against the key's.
func (v *Verifier) Verify() error { return v.s.Verify() }

func (m *Stream) resetChunk(start int64) {
	copy(m.chunkMAC[:8], m.key.Nonce)
	copy(m.chunkMAC[8:], m.key.Nonce)
	m.chunkStart = start
	m.partialN = 0
}

func (m *Stream) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if n > 0 {
		m.ctr.XORKeyStream(p[:n], p[:n])
		m.macWrite(p[:n])
	}
	return n, err
}

// macWrite feeds plaintext into the MAC according to chunk boundaries.
func (m *Stream) macWrite(p []byte) {
	for len(p) > 0 {
		end := ChunkEnd(m.pos)
		n := int64(len(p))
		if room := end - m.pos; room < n {
			n = room
		}
		m.absorb(p[:n])
		m.pos += n
		p = p[n:]
		if m.pos == end {
			m.finishChunk()
			m.resetChunk(m.pos)
		}
	}
}

// absorb appends bytes to the open chunk; when a 16-byte block fills up it
// takes a CBC-MAC step.
func (m *Stream) absorb(p []byte) {
	for len(p) > 0 {
		take := aes.BlockSize - m.partialN
		if take > len(p) {
			take = len(p)
		}
		copy(m.partial[m.partialN:], p[:take])
		m.partialN += take
		p = p[take:]
		if m.partialN == aes.BlockSize {
			m.macBlock(m.partial[:])
			m.partialN = 0
		}
	}
}

func (m *Stream) macBlock(b []byte) {
	for i := 0; i < aes.BlockSize; i++ {
		m.chunkMAC[i] ^= b[i]
	}
	m.block.Encrypt(m.chunkMAC[:], m.chunkMAC[:])
}

// finishChunk closes the open chunk: a partial block is zero padded and the
// chunk MAC is folded into the file MAC.
func (m *Stream) finishChunk() {
	if m.partialN > 0 {
		for i := m.partialN; i < aes.BlockSize; i++ {
			m.partial[i] = 0
		}
		m.macBlock(m.partial[:])
		m.partialN = 0
	}
	for i := 0; i < aes.BlockSize; i++ {
		m.fileMAC[i] ^= m.chunkMAC[i]
	}
	m.block.Encrypt(m.fileMAC[:], m.fileMAC[:])
}

// State serializes the state for resume. Fixed layout, version byte first.
func (m *Stream) State() []byte {
	out := make([]byte, stateLen)
	out[0] = stateVersion
	binary.BigEndian.PutUint64(out[1:9], uint64(m.pos))
	copy(out[9:25], m.fileMAC[:])
	copy(out[25:41], m.chunkMAC[:])
	binary.BigEndian.PutUint64(out[41:49], uint64(m.chunkStart))
	out[49] = byte(m.partialN)
	copy(out[50:66], m.partial[:])
	return out
}

func (m *Stream) restore(saved []byte) error {
	if len(saved) != stateLen {
		return fmt.Errorf("%d bytes, expected %d", len(saved), stateLen)
	}
	if saved[0] != stateVersion {
		return fmt.Errorf("version %d, expected %d", saved[0], stateVersion)
	}
	m.pos = int64(binary.BigEndian.Uint64(saved[1:9]))
	copy(m.fileMAC[:], saved[9:25])
	copy(m.chunkMAC[:], saved[25:41])
	m.chunkStart = int64(binary.BigEndian.Uint64(saved[41:49]))
	m.partialN = int(saved[49])
	if m.partialN >= aes.BlockSize {
		return fmt.Errorf("partial block is %d bytes, must be less than 16", m.partialN)
	}
	copy(m.partial[:], saved[50:66])
	return nil
}

// Verify compares the meta-MAC against the expected one once the stream ends.
func (m *Stream) Verify() error {
	// Close the open chunk if there is one. If the file ended exactly on a
	// chunk boundary the chunk is already closed and no empty chunk may be
	// folded in.
	if m.pos > m.chunkStart {
		m.finishChunk()
		m.resetChunk(m.pos)
	}
	got := metaMAC(m.fileMAC)
	if !bytes.Equal(got, m.key.MAC) {
		return fmt.Errorf("meta-MAC mismatch: expected %x, computed %x", m.key.MAC, got)
	}
	return nil
}

// metaMAC reduces the 16-byte file MAC to mega's 8-byte form.
func metaMAC(f [16]byte) []byte {
	out := make([]byte, 8)
	for i := 0; i < 4; i++ {
		out[i] = f[i] ^ f[4+i]
		out[4+i] = f[8+i] ^ f[12+i]
	}
	return out
}

// MetaMACOf computes the meta-MAC of a plaintext. In tests it produces the
// expected value and is used to pack the key.
func MetaMACOf(aesKey, nonce, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	m := &Stream{block: block, key: Key{AES: aesKey, Nonce: nonce}}
	m.resetChunk(0)
	m.macWrite(plain)
	if m.pos > m.chunkStart {
		m.finishChunk()
	}
	return metaMAC(m.fileMAC), nil
}

// EncryptCTR encrypts plaintext with the given key and nonce. CTR is
// symmetric, so this is the same operation as decryption; a separate name for
// tests and the fake server.
func EncryptCTR(aesKey, nonce, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, aes.BlockSize)
	copy(iv, nonce)
	out := make([]byte, len(plain))
	cipher.NewCTR(block, iv).XORKeyStream(out, plain)
	return out, nil
}
