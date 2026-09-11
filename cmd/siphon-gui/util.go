package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// normalizeDir, kullanıcıdan veya klasör seçiciden gelen yolu Windows'un
// beklediği biçime çevirir.
//
// ÖLÇÜLDÜ: Fyne'ın klasör seçicisi yolu URI'den türetiyor ve EĞİK ÇİZGİYLE
// veriyor — "E:\x" seçince kutuya "E:/x" yazılıyor. Go'nun dosya çağrıları
// eğik çizgiyi kabul ettiği için indirme doğru yere iniyor ve hata görünmez
// kalıyor; ama explorer.exe kabul etmiyor. Yolu tanımayınca hata da vermiyor,
// sessizce Belgeler klasörünü açıyor. Kullanıcının gördüğü davranış buydu.
//
// Clean ayrıca sondaki ayracı atıyor ve bu ikinci bir tuzağı kapatıyor:
// "E:\x\" komut satırında explorer "E:\x\" olarak tırnaklanır, sondaki ters
// çizgi kapanış tırnağını kaçırır ve explorer yine yolu tanımaz.
//
// Tırnaklar da soyuluyor: Windows'un "Yol olarak kopyala" komutu yolu
// tırnak içinde veriyor ve yapıştıran herkes bunu fark etmiyor.
func normalizeDir(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"`)
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Çıplak sürücü harfi ("E:") SÜRÜCÜYE GÖRELİ bir yoldur, kökü değil:
	// Clean onu "E:." yapar, yani "E: sürücüsünün geçerli dizini". Bu, işlem
	// durumuna bağlı bir yer; klasör kutusuna "E:" yazan kimse bunu kastetmez.
	// Kök olarak yorumluyoruz.
	//
	// VolumeName girdinin TAMAMINA eşitse elde yalnızca sürücü harfi var
	// demektir ("E:"). "E:\", "E:/x" ve UNC yolları eşit olmaz, dokunulmaz.
	if s == filepath.VolumeName(s) {
		s += string(filepath.Separator)
	}
	return filepath.Clean(filepath.FromSlash(s))
}

// defaultOutDir, makul bir başlangıç klasörü seçer.
func defaultOutDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		d := filepath.Join(home, "Downloads")
		if fi, serr := os.Stat(d); serr == nil && fi.IsDir() {
			return filepath.Join(d, "siphon")
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// pickOpenTarget, "Klasörü aç" için en anlamlı hedefi seçer.
//
// Sıra: son inen dosya (klasöründe SEÇİLİ) -> başlayan albümün klasörü ->
// çıktı kökü. Çıktı kökü "E:\" iken dosyalar "E:\Albüm\" altına indiği için
// kökü açmak kullanıcıya "yanlış klasör açıldı" görünüyordu; IDM'in yaptığı
// gibi dosyanın kendisine gitmek doğru davranış.
//
// Klasör SESSİZCE OLUŞTURULMUYOR: yoksa sebebi söyleniyor.
func pickOpenTarget(lastPath, lastDir, outDir string) (target string, selectFile bool, reason string) {
	if lastPath != "" {
		if fi, err := os.Stat(lastPath); err == nil && !fi.IsDir() {
			return lastPath, true, ""
		}
	}
	if lastDir != "" {
		if fi, err := os.Stat(lastDir); err == nil && fi.IsDir() {
			return lastDir, false, ""
		}
	}
	if outDir == "" {
		return "", false, ""
	}
	fi, err := os.Stat(outDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false, "Klasör henüz yok: " + outDir + " — indirme başlayınca oluşacak."
	case err != nil:
		return "", false, "Klasöre erişilemedi: " + err.Error()
	case !fi.IsDir():
		return "", false, "Bu bir klasör değil: " + outDir
	}
	// Göreli yol verilirse explorer onu KENDİ çalışma dizinine göre çözerdi.
	if abs, aerr := filepath.Abs(outDir); aerr == nil {
		outDir = abs
	}
	return outDir, false, ""
}

// parseLinks, metin alanını URL listesine çevirir.
// Komut satırındaki -i dosyası ile AYNI kurallar: boş satır ve # atlanır.
func parseLinks(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func shortName(s string) string {
	s = filepath.Base(s)
	const max = 60
	if len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-3]) + "..."
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func humanBytes(n int64) string {
	if n < 0 {
		return "? B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	for _, u := range units {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f PB", v/unit)
}

// parseSpeedLimit, "2", "2.5", "0" gibi MB/s girdisini bayt/saniyeye çevirir.
// Boş veya 0 sınırsız demek; anlaşılmayan girdi hata döner, sessizce 0 sayılmaz.
func parseSpeedLimit(s string) (int64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if s == "" {
		return 0, nil
	}
	var mbps float64
	if _, err := fmt.Sscanf(s, "%g", &mbps); err != nil {
		return 0, fmt.Errorf("hız sınırı anlaşılamadı: %q", s)
	}
	if mbps < 0 {
		return 0, fmt.Errorf("hız sınırı negatif olamaz")
	}
	return int64(mbps * 1024 * 1024), nil
}
