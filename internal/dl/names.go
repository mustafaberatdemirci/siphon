package dl

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// MaxComponentUTF16, bir yol BİLEŞENİNİN (klasör adı veya dosya adı)
// taşıyabileceği en fazla UTF-16 kod birimi sayısıdır.
//
// Windows'ta iki ayrı uzunluk sınırı var ve ikisi karıştırılıyor:
//
//	MAX_PATH (260)  — TÜM yolun uzunluğu. Go'nun os paketi uzun MUTLAK yollar
//	                  için `\\?\` dönüşümünü kendisi yapıyor, yani bu sınır
//	                  büyük ölçüde çözülmüş durumda.
//	255             — NTFS'in BİLEŞEN başına sınırı, UTF-16 kod birimi olarak.
//	                  `\\?\` öneki bunu AŞMAZ; hiçbir numara aşmaz.
//
// Bu yüzden kırpma bileşen seviyesinde yapılmak zorunda: albüm klasör adı ve
// dosya adı ayrı ayrı bu sınıra sığdırılır.
//
// `\\?\` önekini elle EKLEMİYORUZ. Eklemek yol normalizasyonunu kapatır: ileri
// eğik çizgi ve `.`/`..` bileşenleri artık çözülmez. os paketine güvenmek doğru.
const MaxComponentUTF16 = 255

// truncHashLen, kırpma sonekindeki onaltılık karakter sayısı.
// Kırpma deterministik olmak zorunda: aynı uzun ad her koşuda aynı kısa ada
// dönüşmeli, yoksa resume ve "zaten var" kontrolü çalışmaz.
const truncHashLen = 8

// maxExtUTF16, kırpmada korunacak uzantının üst sınırı. Patolojik bir "uzantı"
// (noktadan sonra 300 karakter) bütçenin tamamını yemesin.
const maxExtUTF16 = 24

// forbidden, Windows'ta dosya adında kullanılamayan karakterler.
// Kontrol karakterleri (0-31) ayrıca ele alınıyor.
const forbidden = `<>:"/\|?*`

// reserved, Windows'un aygıt adlarıdır. Bu adlarla dosya açmak dosya değil
// AYGIT açar; `CON.txt` de dahil, çünkü eşleştirme ilk noktadan öncesi
// üzerinden ve büyük/küçük harf duyarsız yapılıyor.
//
// Üst simgeli varyantlar (COM¹, LPT²) bazı kod sayfalarında normal rakama
// çözüldüğü için ayrıca listelenmiş durumda.
//
// COM0 ve LPT0 KASITLI olarak yok: klasik olarak ayrılmış değiller ve
// gereksiz yeniden adlandırma, gerçek dosya adını bozmak demek.
var reserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"conin$": true, "conout$": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
	"com¹": true, "com²": true, "com³": true,
	"lpt¹": true, "lpt²": true, "lpt³": true,
}

// Component, tek bir yol bileşenini Windows'ta güvenli hale getirir ve tam
// 255 UTF-16 birim sınırına sığdırır. Klasör adları için doğru olan budur.
//
// DOSYA adları için ComponentLimit kullanılmak zorunda: indirici nihai adın
// sonuna `.part` ve `.part.state` ekliyor, yani 255'i nihai ada harcamak
// bileşeni 266 birime çıkarır ve NTFS isteği reddeder.
func Component(name string) string {
	return ComponentLimit(name, MaxComponentUTF16)
}

// ComponentLimit, Component ile aynıdır ama uzunluk sınırını çağıran verir.
// Çağıran, adın sonuna kendi ekleyeceği soneklere yer ayırmak zorunda.
//
// Kullanılabilir hiçbir şey kalmazsa "" döner; karar çağırana ait.
func ComponentLimit(name string, limit int) string {
	s := replaceForbidden(name)

	// Sondaki nokta ve boşluk: Windows bunları SESSİZCE atar. Biz atmazsak
	// "dosya. " yazıp "dosya" oluşur, sonra Stat("dosya. ") tutar ama rename
	// ve "zaten var" kontrolü beklenmedik biçimde davranır.
	s = strings.TrimRight(s, ". ")
	s = strings.TrimLeft(s, " ")

	if s == "" || s == "." || s == ".." {
		return ""
	}

	s = escapeReserved(s)
	s = truncateUTF16(s, name, limit)
	// Kırpma sonrası yeniden kontrol: kenar durumda sonda boşluk/nokta kalmasın.
	s = strings.TrimRight(s, ". ")
	if s == "" {
		return ""
	}
	return s
}

func replaceForbidden(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r < 0x20, r == 0x7f:
			// Kontrol karakterleri tamamen atılır; "-" koymak gürültü üretir.
		case strings.ContainsRune(forbidden, r):
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeReserved, ayrılmış aygıt adlarının başına alt çizgi koyar.
// Eşleştirme ilk noktadan ÖNCESİ üzerinden yapılıyor: "CON.txt" da ayrılmıştır.
func escapeReserved(s string) string {
	stem := s
	if i := strings.IndexByte(s, '.'); i >= 0 {
		stem = s[:i]
	}
	// Windows aygıt adı çözümlemesinde sondaki boşluklar yok sayılır.
	stem = strings.TrimRight(stem, " ")
	if reserved[strings.ToLower(stem)] {
		return "_" + s
	}
	return s
}

// utf16Len, bir dizenin UTF-16 kod birimi uzunluğunu döndürür.
// BMP dışı runeler (emoji gibi) vekil çift olarak İKİ birim sayılır; NTFS
// sınırı rune değil kod birimi üzerinden işliyor.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xffff {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// truncPrefix, bir dizeyi en fazla limit UTF-16 birimine kırpar.
// Rune sınırında keser, yani vekil çift asla yarılmaz.
func truncPrefix(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	n := 0
	for i, r := range s {
		w := 1
		if r > 0xffff {
			w = 2
		}
		if n+w > limit {
			return s[:i]
		}
		n += w
	}
	return s
}

// truncateUTF16, bileşeni 255 UTF-16 birimine sığdırır.
//
// Kırpma hash sonekiyle yapılır: aynı klasörde ilk 200 karakteri aynı olan iki
// uzun ad, kırpıldığında AYNI ada dönüşür ve biri diğerini ezer. Soneki orijinal
// adın sha256'sından türetmek bunu engeller ve deterministik tutar, yani resume
// ile "zaten var" kontrolü koşular arasında çalışmaya devam eder.
//
// original, hash'in kaynağıdır: temizlenmiş hali değil HAM ad kullanılır ki
// temizleme kuralları değişse bile aynı kaynak ad aynı hash'i üretsin.
func truncateUTF16(s, original string, limit int) string {
	if utf16Len(s) <= limit {
		return s
	}

	ext := filepath.Ext(s)
	if utf16Len(ext) > maxExtUTF16 {
		ext = ""
	}
	stem := strings.TrimSuffix(s, ext)

	sum := sha256.Sum256([]byte(original))
	suffix := "~" + hex.EncodeToString(sum[:])[:truncHashLen]

	budget := limit - utf16Len(suffix) - utf16Len(ext)
	stem = truncPrefix(stem, budget)
	// Kırpma sonda boşluk veya nokta bırakmış olabilir.
	stem = strings.TrimRight(stem, ". ")

	return stem + suffix + ext
}
