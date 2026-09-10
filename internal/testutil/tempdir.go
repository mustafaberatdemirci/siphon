// Package testutil, testler arasında paylaşılan yardımcıları barındırır.
//
// Yalnızca _test.go dosyalarından import ediliyor, bu yüzden üretim
// binary'sine girmiyor.
package testutil

import (
	"os"
	"testing"
	"time"
)

// TempDir, t.TempDir() yerine kullanılır.
//
// Windows'ta antivirüs ve arama indeksleyicisi yeni yazılmış dosyalara kısa
// süre tutunuyor; t.TempDir()'in kayıtlı RemoveAll'ı o anda "Dizin boş değil"
// ile düşüyor ve test, gövdesindeki tüm iddialar geçmiş olmasına rağmen FAIL
// görünüyor. İndirme testleri tam olarak sessiz veri bozulmasını kovaladığı
// için ortam kaynaklı bir flake burada özellikle pahalı: kırmızı bir suite'te
// gerçek bir regresyonu kimse fark etmez.
//
// Çözüm silmeyi tekrar denemek. Yine olmazsa test düşürülmüyor, yalnızca not
// bırakılıyor: geçici klasörün silinememesi aracın davranışıyla ilgili bir şey
// söylemiyor.
func TempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "siphon-test-")
	if err != nil {
		t.Fatalf("geçici klasör açılamadı: %v", err)
	}
	t.Cleanup(func() {
		const attempts = 25
		for i := 0; i < attempts; i++ {
			if rerr := os.RemoveAll(dir); rerr == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Logf("geçici klasör silinemedi (Windows dosya kilidi), elle temizlenebilir: %s", dir)
	})
	return dir
}
