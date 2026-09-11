package dl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
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

// --- Sahte cozucu ---
//
// Konuma bagli XOR: her bayt kendi konumundan turetilen bir anahtarla
// sifreleniyor. Boylece cozucu offset'i BILMEK zorunda; resume yanlis yerden
// baslarsa cikti bozulur ve test yakalar. Durum = konum + duz metin toplami;
// Verify toplami beklenenle karsilastiriyor (meta-MAC'in kucuk kardesi).

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
		return errors.New("kasitli dogrulama hatasi")
	}
	if f.sum != f.wantSum {
		return fmt.Errorf("toplam %d, beklenen %d", f.sum, f.wantSum)
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
			return nil, errors.New("cozucu durumu yok")
		}
		fs.pos = int64(binary.BigEndian.Uint64(saved[:8]))
		fs.sum = binary.BigEndian.Uint32(saved[8:])
		if fs.pos != offset {
			return nil, fmt.Errorf("durum %d diyor, offset %d", fs.pos, offset)
		}
	}
	return fs, nil
}

// encodedRangeServer, SIFRELI govdeyi Range/If-Range destegiyle sunar.
func encodedRangeServer(t *testing.T, etag string) *httptest.Server {
	t.Helper()
	enc := fakeEncode(payload)
	modtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		http.ServeContent(w, r, "veri.bin", modtime, bytes.NewReader(enc))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// encodedSlowServer, sifreli govdenin ilk parcasini verip release'e kadar bekler.
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

// --- Testler ---

// Cozucu takiliyken diske DUZ METIN yazilmali ve sha256 duz metnin olmali.
func TestDecodeHookProducesPlaintext(t *testing.T) {
	srv := encodedRangeServer(t, `"v1"`)
	out := tempDir(t)
	dec := &fakeDecoder{wantSum: fakeSum(payload)}
	d := &Downloader{Client: srv.Client(), Decode: dec.decode}

	it := testItem(srv.URL+"/veri.bin", "veri.bin")
	it.SHA256 = payloadSHA() // DUZ metnin hash'i
	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, payload) {
		t.Fatal("diske yazilan icerik duz metin degil")
	}
	if res.SHA256 != payloadSHA() {
		t.Errorf("sha256 duz metnin olmali: %s", res.SHA256)
	}
}

// Kesinti sonrasi resume: cozucu KAYDEDILEN durumla, dogru offset'ten
// kurulmali. Konuma bagli XOR sayesinde yanlis bir baslangic icerigi bozar.
func TestDecodeResumeRestoresDecoderState(t *testing.T) {
	release := make(chan struct{})
	slow := encodedSlowServer(t, 20000, release)
	out := tempDir(t)
	dec := &fakeDecoder{wantSum: fakeSum(payload)}

	ctx, cancel := context.WithCancel(context.Background())
	d := &Downloader{Client: slow.Client(), Decode: dec.decode}
	it := testItem(slow.URL+"/veri.bin", "veri.bin")
	it.SHA256 = payloadSHA()
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	_, err := d.Download(ctx, out, it)
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("context.Canceled bekleniyordu: %v", err)
	}

	st := readState(t, filepath.Join(out, "veri.bin.part.state"))
	if st.Offset <= 0 {
		t.Fatal("offset ilerlememis")
	}
	if len(st.DecoderState) == 0 {
		t.Fatal("cozucu durumu state'e yazilmadi; resume mumkun degil")
	}
	if pos := int64(binary.BigEndian.Uint64(st.DecoderState[:8])); pos != st.Offset {
		t.Fatalf("cozucu durumu %d diyor, offset %d: ikisi ayni anda alinmiyor", pos, st.Offset)
	}

	srv := encodedRangeServer(t, `"v1"`)
	d2 := &Downloader{Client: srv.Client(), Decode: dec.decode}
	it2 := testItem(srv.URL+"/veri.bin", "veri.bin")
	it2.SHA256 = payloadSHA()
	if _, err := d2.Download(context.Background(), out, it2); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(out, "veri.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("resume sonrasi icerik bozuk: cozucu yanlis yerden basladi")
	}

	last := dec.construct[len(dec.construct)-1]
	if last.offset != st.Offset || last.saved == 0 {
		t.Errorf("resume'da cozucu offset=%d saved=%d ile kuruldu; %d ve dolu durum bekleniyordu",
			last.offset, last.saved, st.Offset)
	}
}

// Verify duserse .part SILINMELI ve hata yeniden denenebilir OLMAMALI:
// anahtar yanlissa tekrar indirmek ayni copu uretir.
func TestDecodeVerifyFailureRemovesPart(t *testing.T) {
	srv := encodedRangeServer(t, `"v1"`)
	out := tempDir(t)
	dec := &fakeDecoder{wantSum: fakeSum(payload), failVery: true}
	d := &Downloader{Client: srv.Client(), Decode: dec.decode}

	_, err := d.Download(context.Background(), out, testItem(srv.URL+"/veri.bin", "veri.bin"))
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("ErrIntegrity bekleniyordu: %v", err)
	}
	var rt interface{ Retryable() bool }
	if errors.As(err, &rt) && rt.Retryable() {
		t.Error("butunluk hatasi yeniden denenebilir sayildi")
	}
	if _, serr := os.Stat(filepath.Join(out, "veri.bin.part")); !os.IsNotExist(serr) {
		t.Error(".part diskte kaldi; her kosu ayni hatayi tekrarlar")
	}
	if _, serr := os.Stat(filepath.Join(out, "veri.bin")); !os.IsNotExist(serr) {
		t.Error("dogrulanamayan icerik nihai ada tasindi")
	}
}

// Cozucu varken durumsuz bir state (eski surumden veya bozuk) ile resume
// GUVENLI DEGIL: sifirdan baslanmali.
func TestDecodeMissingStateForcesRestart(t *testing.T) {
	srv := encodedRangeServer(t, `"v1"`)
	out := tempDir(t)

	// Elle: yarim .part + cozucu durumu OLMAYAN state.
	part := filepath.Join(out, "veri.bin.part")
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
	it := testItem(srv.URL+"/veri.bin", "veri.bin")
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(dec.construct) == 0 || dec.construct[0].offset != 0 {
		t.Fatalf("cozucu sifirdan kurulmaliydi: %+v", dec.construct)
	}
	got, _ := os.ReadFile(filepath.Join(out, "veri.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("icerik bozuk")
	}
}

// Cozucu YOKKEN davranis degismemeli: state'te cozucu alani bos kalmali.
func TestNoDecoderLeavesStateUntouched(t *testing.T) {
	release := make(chan struct{})
	slow := slowServer(t, 20000, release)
	out := tempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	d := &Downloader{Client: slow.Client()}
	_, _ = d.Download(ctx, out, testItem(slow.URL+"/veri.bin", "veri.bin"))
	close(release)

	st := readState(t, filepath.Join(out, "veri.bin.part.state"))
	if len(st.DecoderState) != 0 {
		t.Errorf("cozucu yokken decoder_state dolu: %d bayt", len(st.DecoderState))
	}
}
