package megacrypto

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

// Bu dosyadaki testler sifreleme/cozme/MAC'in KENDI ICINDE tutarli oldugunu
// kanitliyor. Semanin mega'nin gercek semasiyla ayni oldugu canli bir dosyayla
// dogrulanmak zorunda; o adim mega_test.go'da degil, elle yapilan olcumde.

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// testMegaKey, rastgele anahtar+nonce uretir ve duz metnin meta-MAC'iyle
// paketler: linkte gelen 32 baytin birebir karsiligi.
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
		t.Fatal("pack/unpack anahtari bozdu")
	}
	if _, err := UnpackFileKey(make([]byte, 16)); err == nil {
		t.Error("16 baytlik dosya anahtari kabul edildi")
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
	at, err := EncryptAttrs(key, Attrs{Name: "Tatil Videosu — Özgür & Aslı.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := DecryptAttrs(key, at)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "Tatil Videosu — Özgür & Aslı.mp4" {
		t.Errorf("ad = %q", a.Name)
	}
	// Yanlis anahtar "MEGA" onekini uretmez: hata net olmali, cop ad degil.
	if _, err := DecryptAttrs(randBytes(t, 16), at); err == nil {
		t.Error("yanlis anahtarla oznitelik 'cozuldu'")
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
		t.Fatal("dugum anahtari bozuldu")
	}
}

// Parca sinirlari mega'nin duzenine gore: 128K, 384K, 768K, 1280K, 1920K,
// 2688K, 3584K, 4608K, sonra her 1M.
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
		// 4.5 MiB'den sonra sinirlar TAM MiB'e hizali DEGIL: 4608K + n*1024K.
		// 100 MiB = 102400K, onu iceren parca 102912K'da (100.5 MiB) biter.
		{100 * 1024 * k, 102912 * k},
	}
	for _, c := range cases {
		if got := ChunkEnd(c.pos); got != c.end {
			t.Errorf("ChunkEnd(%d) = %d, %d bekleniyordu", c.pos, got, c.end)
		}
	}
}

// Tam akis: sifrele -> coz -> dogrula. Boyut, parca sinirlarini ve yarim
// bloklari zorlayacak sekilde secildi (birden fazla parca, 16'nin kati degil).
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
		t.Fatal("cozulen icerik duz metinle ayni degil")
	}
	if err := ms.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// Dosya tam bir parca sinirinda bitiyorsa bos parca katlanmamali.
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

// Tek bir bayt degisirse dogrulama DUSMELI. Bu, bozuk indirmenin "basarili"
// sayilmasini engelleyen tek kontrol: mega sha256 vermiyor.
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
		t.Fatal("bozuk icerik dogrulamayi gecti")
	}
}

// Resume: akis rastgele bir yerde kesilip kaydedilen durumla devam edince
// hem icerik hem MAC dogru olmali. Kesme noktalari kasitli olarak zor secildi:
// blok ortasi, parca ortasi, tam parca siniri, ilk bayttan sonra.
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
			t.Fatalf("cut=%d: resume kurulamadi: %v", cut, err)
		}
		tail, _ := io.ReadAll(second)

		if got := append(head, tail...); !bytes.Equal(got, plain) {
			t.Fatalf("cut=%d: resume sonrasi icerik bozuk", cut)
		}
		if err := second.Verify(); err != nil {
			t.Fatalf("cut=%d: resume sonrasi Verify: %v", cut, err)
		}
	}
}

// Durum ile offset uyusmuyorsa cozucu KURULMAMALI; sessizce yanlis yerden
// baslamak dogrulamanin en sonda dusmesi demek, yani tum indirme bosa gider.
func TestMegaStreamRejectsMismatchedState(t *testing.T) {
	plain := randBytes(t, 50*1024)
	key, _ := testMegaKey(t, plain)
	enc, _ := EncryptCTR(key.AES, key.Nonce, plain)

	first, _ := NewStream(key, 0, nil, bytes.NewReader(enc[:1000]))
	_, _ = io.ReadAll(first)
	saved := first.State()

	if _, err := NewStream(key, 2000, saved, bytes.NewReader(enc[2000:])); err == nil {
		t.Error("durum 1000 derken 2000'den resume kabul edildi")
	}
	if _, err := NewStream(key, 1000, nil, bytes.NewReader(enc[1000:])); err == nil {
		t.Error("durumsuz resume kabul edildi")
	}
	if _, err := NewStream(key, 1000, saved[:10], bytes.NewReader(enc[1000:])); err == nil {
		t.Error("kirpilmis durum kabul edildi")
	}
}

// State() her cagrida o ana kadar okunan bayt sayisini yansitmali: indirici
// bunu offset ile ayni anda aliyor.
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
			t.Fatalf("State pos=%d, okunan=%d", probe.pos, read)
		}
		if err == io.EOF {
			break
		}
	}
}
