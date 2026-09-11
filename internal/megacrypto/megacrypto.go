// Package megacrypto, mega.nz'nin istemci tarafı şifreleme şemasını uygular.
// Tamamı standart kütüphane; site protokolünden bağımsız olduğu için ayrı
// paket: hem çözümleyici hem de sahte sunucu kuran testler kullanıyor.
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

// mega'nın istemci tarafı şifrelemesi. Hepsi standart kütüphane; mega'ya özgü
// olan yalnızca anahtarın nasıl paketlendiği, özniteliklerin nasıl sarıldığı
// ve bütünlük MAC'inin parça sınırları.
//
// Kaynak: mega'nın kendi web istemcisi ve bağımsız uygulamalar (mega.py,
// megatools, plowshare) aynı şemayı anlatıyor:
//   - içerik: AES-128-CTR, IV = nonce(8) || blok sayacı(8, big-endian)
//   - öznitelik: AES-128-CBC, sıfır IV, "MEGA" öneki + JSON + sıfır dolgu
//   - bütünlük: parça başına CBC-MAC (IV = nonce||nonce), parça MAC'leri
//     yine CBC-MAC ile katlanır, 16 bayt dosya MAC'i 8 bayta XOR'lanır
//
// Bu dosyadaki testler kendi içinde tutarlılığı kanıtlıyor; şemanın mega'yla
// birebir aynı olduğu CANLI bir dosyayla doğrulanmak zorunda. Bunu XOR
// çözümünde de yapmıştık: bağımsız bir uygulamayla bayt bayt karşılaştırma.

// B64Decode, mega'nın URL-güvenli, dolgusuz base64'ünü çözer. Eski
// linkler '=' yerine ',' kullanabiliyor; standart alfabe de kabul ediliyor.
func B64Decode(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer("+", "-", "/", "_", "=", "", ",", "").Replace(s)
	return base64.RawURLEncoding.DecodeString(s)
}

func B64Encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Key, 32 baytlık dosya anahtarından türeyen malzeme.
type Key struct {
	AES   []byte // 16: içerik ve öznitelik anahtarı
	Nonce []byte // 8: CTR sayacının üst yarısı ve MAC IV'si
	MAC   []byte // 8: beklenen meta-MAC
}

// UnpackFileKey, linkteki 32 baytı açar: ilk 16 ile son 16 XOR'lanınca
// AES anahtarı çıkar; son 16'nın kendisi nonce(8) + meta-MAC(8).
func UnpackFileKey(full []byte) (Key, error) {
	if len(full) != 32 {
		return Key{}, fmt.Errorf("dosya anahtarı %d bayt, 32 bekleniyordu", len(full))
	}
	k := Key{AES: make([]byte, 16), Nonce: make([]byte, 8), MAC: make([]byte, 8)}
	for i := 0; i < 16; i++ {
		k.AES[i] = full[i] ^ full[16+i]
	}
	copy(k.Nonce, full[16:24])
	copy(k.MAC, full[24:32])
	return k, nil
}

// PackFileKey, unpack'in tersi. Testlerde ve klasör düğümlerinin
// anahtarını Item.Secret'a koyarken kullanılıyor.
func PackFileKey(aesKey, nonce, mac []byte) []byte {
	full := make([]byte, 32)
	copy(full[16:24], nonce)
	copy(full[24:32], mac)
	for i := 0; i < 16; i++ {
		full[i] = aesKey[i] ^ full[16+i]
	}
	return full
}

// Attrs, şifreli öznitelik bloğunun içi. Şimdilik yalnızca ad gerekiyor.
type Attrs struct {
	Name string `json:"n"`
}

// DecryptAttrs, "at" alanını çözer. Yanlış anahtarla çözülen blok "MEGA"
// ile başlamaz; bu, anahtarın doğruluğunu içeriği indirmeden test etmenin
// ucuz yolu.
func DecryptAttrs(aesKey []byte, at string) (Attrs, error) {
	raw, err := B64Decode(at)
	if err != nil {
		return Attrs{}, fmt.Errorf("öznitelik base64 değil: %w", err)
	}
	if len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		return Attrs{}, fmt.Errorf("öznitelik uzunluğu %d, 16'nın katı değil", len(raw))
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return Attrs{}, err
	}
	cipher.NewCBCDecrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(raw, raw)
	if !bytes.HasPrefix(raw, []byte("MEGA")) {
		return Attrs{}, errors.New("öznitelik çözülemedi: anahtar yanlış olabilir")
	}
	js := bytes.TrimRight(raw[4:], "\x00")
	var a Attrs
	if err := json.Unmarshal(js, &a); err != nil {
		return Attrs{}, fmt.Errorf("öznitelik JSON değil: %w", err)
	}
	return a, nil
}

// EncryptAttrs, DecryptAttrs'ın tersi; testler ve sahte sunucu için.
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

// DecryptNodeKey, klasör linkindeki düğümlerin anahtarını açar: düğüm
// anahtarı, klasör anahtarıyla AES-ECB (blok blok) şifrelenmiş halde geliyor.
func DecryptNodeKey(folderKey, enc []byte) ([]byte, error) {
	if len(enc) == 0 || len(enc)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("düğüm anahtarı %d bayt, 16'nın katı değil", len(enc))
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

// EncryptNodeKey, DecryptNodeKey'in tersi; testler için.
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

// ChunkEnd, pos konumunu içeren MAC parçasının bitişini (hariç) verir.
//
// mega'nın parça düzeni: ilk sekiz parça 128K, 256K, ..., 1M büyüklüğünde
// (her biri bir öncekinden 128K büyük), sonrası hep 1M. Sınırlar mutlak
// konumdan türediği için resume'da yeniden kurulabiliyor.
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

// Stream, şifreli gövdeyi çözen ve bütünlük MAC'ini yürüten okuyucu.
// site.DecodedStream ile aynı üç metodu (Read, State, Verify) sunar.
type Stream struct {
	r     io.Reader
	block cipher.Block
	ctr   cipher.Stream
	key   Key

	pos        int64    // düz metin konumu, dosya başından
	fileMAC    [16]byte // biten parçaların katlanmış MAC'i
	chunkMAC   [16]byte // açık parçanın CBC-MAC durumu
	chunkStart int64    // açık parçanın başlangıcı
	partial    [16]byte // açık parçanın tamamlanmamış son bloğu
	partialN   int
}

const stateVersion = 1

// stateLen: sürüm(1) + pos(8) + fileMAC(16) + chunkMAC(16) + chunkStart(8)
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
		// Sıfırdan farklı bir yerden başlıyorsak MAC durumu kaydedilmiş olmak
		// ZORUNDA; yoksa doğrulama baştan itibaren yanlış olur ve bunu ancak
		// dosya bitince, tüm indirme boşa gittikten sonra öğreniriz.
		if err := m.restore(saved); err != nil {
			return nil, fmt.Errorf("mega çözücü durumu: %w", err)
		}
		if m.pos != offset {
			return nil, fmt.Errorf("mega çözücü durumu %d bayt diyor, resume %d'den istendi", m.pos, offset)
		}
	}

	// CTR konumu: sayaç 16 baytlık blok cinsinden, blok içi kalan atlanıyor.
	iv := make([]byte, aes.BlockSize)
	copy(iv, key.Nonce)
	binary.BigEndian.PutUint64(iv[8:], uint64(offset/aes.BlockSize))
	m.ctr = cipher.NewCTR(block, iv)
	if skip := int(offset % aes.BlockSize); skip > 0 {
		scratch := make([]byte, skip)
		m.ctr.XORKeyStream(scratch, scratch)
	}
	return m, nil
}

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

// macWrite, düz metni parça sınırlarına göre MAC'e işler.
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

// absorb, açık parçaya bayt ekler; 16'lık bloklar dolunca CBC-MAC adımı atar.
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

// finishChunk, açık parçayı kapatır: yarım blok sıfırla doldurulur, parça
// MAC'i dosya MAC'ine katlanır.
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

// State, resume için durumu serileştirir. Sabit yerleşim, sürüm baytı önde.
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
		return fmt.Errorf("%d bayt, %d bekleniyordu", len(saved), stateLen)
	}
	if saved[0] != stateVersion {
		return fmt.Errorf("sürüm %d, %d bekleniyordu", saved[0], stateVersion)
	}
	m.pos = int64(binary.BigEndian.Uint64(saved[1:9]))
	copy(m.fileMAC[:], saved[9:25])
	copy(m.chunkMAC[:], saved[25:41])
	m.chunkStart = int64(binary.BigEndian.Uint64(saved[41:49]))
	m.partialN = int(saved[49])
	if m.partialN >= aes.BlockSize {
		return fmt.Errorf("yarım blok %d bayt, 16'dan küçük olmalı", m.partialN)
	}
	copy(m.partial[:], saved[50:66])
	return nil
}

// Verify, akış bitince meta-MAC'i beklenenle karşılaştırır.
func (m *Stream) Verify() error {
	// Açık parça varsa kapat. Dosya tam bir parça sınırında bitmişse parça
	// zaten kapanmıştır ve boş bir parça katlanmamalı.
	if m.pos > m.chunkStart {
		m.finishChunk()
		m.resetChunk(m.pos)
	}
	got := metaMAC(m.fileMAC)
	if !bytes.Equal(got, m.key.MAC) {
		return fmt.Errorf("meta-MAC uyuşmuyor: beklenen %x, hesaplanan %x", m.key.MAC, got)
	}
	return nil
}

// metaMAC, 16 baytlık dosya MAC'ini mega'nın 8 baytlık biçimine indirir.
func metaMAC(f [16]byte) []byte {
	out := make([]byte, 8)
	for i := 0; i < 4; i++ {
		out[i] = f[i] ^ f[4+i]
		out[4+i] = f[8+i] ^ f[12+i]
	}
	return out
}

// MetaMACOf, düz metnin meta-MAC'ini hesaplar. Testlerde beklenen değeri
// üretmek ve anahtarı paketlemek için.
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

// EncryptCTR, düz metni verilen anahtar ve nonce ile şifreler. CTR simetrik
// olduğu için çözmeyle aynı işlem; testler ve sahte sunucu için ayrı ad.
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
