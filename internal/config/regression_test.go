package config

// Kod incelemesinde bulunan config hataları için regresyon testleri.

import (
	"strings"
	"testing"
)

// BULGU 10a. remove() ham string karşılaştırıyordu, eşleştirme tarafı
// (site.MatchHost) ise host'u küçük harfe çevirip sondaki noktayı atıyor.
// Sonuç: `domains_remove = ["PixelDrain.COM"]` yazan bir silme sessizce
// hiçbir şey yapmıyordu.
func TestDomainsRemoveIsCaseAndDotInsensitive(t *testing.T) {
	cases := []string{"PixelDrain.COM", "pixeldrain.com.", "  PIXELDRAIN.com  "}
	for _, form := range cases {
		ext := "[[site]]\nname = \"pixeldrain\"\ndomains_remove = [\"" + form + "\"]\n"
		cfgs, _, err := Load([]byte(baseTOML), writeTemp(t, ext))
		if err != nil {
			t.Fatalf("%q: Load: %v", form, err)
		}
		for _, d := range cfgs[0].Domains {
			if strings.EqualFold(strings.TrimSuffix(d, "."), "pixeldrain.com") {
				t.Errorf("%q ile silme çalışmadı; domains = %v", form, cfgs[0].Domains)
			}
		}
	}
}

// BULGU 10b. Silme mergeSite içinde uygulanıyordu, ama dış dosyanın YENİ
// tanımladığı bir site o dala hiç girmiyor. Sonuç: yeni sitenin kendi
// domains_remove alanı sessizce yok sayılıyordu.
func TestNewSiteOwnDomainsRemoveIsApplied(t *testing.T) {
	ext := `
[[site]]
name = "yenisite"
domains = ["bir.com", "iki.com", "uc.com"]
domains_remove = ["iki.com"]
`
	cfgs, _, err := Load([]byte(baseTOML), writeTemp(t, ext))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var found *struct{ domains []string }
	for _, c := range cfgs {
		if c.Name == "yenisite" {
			found = &struct{ domains []string }{c.Domains}
		}
	}
	if found == nil {
		t.Fatal("yenisite kurulmadı")
	}
	for _, d := range found.domains {
		if d == "iki.com" {
			t.Fatalf("yeni sitenin domains_remove'u uygulanmadı: %v", found.domains)
		}
	}
	if len(found.domains) != 2 {
		t.Fatalf("domains = %v, iki eleman bekleniyordu", found.domains)
	}
}

// Silme tüm domainleri süpürürse bu bir kullanım hatasıdır: domains boş bir
// resolver hiçbir URL'e uymaz ve araç sessizce hiçbir şey indirmez.
func TestRemovingAllDomainsIsUsageError(t *testing.T) {
	ext := `
[[site]]
name = "pixeldrain"
domains_remove = ["pixeldrain.com", "pixeldra.in"]
`
	_, _, err := Load([]byte(baseTOML), writeTemp(t, ext))
	if err == nil {
		t.Fatal("tüm domainler silindiğinde hata bekleniyordu")
	}
}

// cdn_patterns_remove de aynı yoldan geçmeli.
func TestCDNPatternsRemoveApplied(t *testing.T) {
	ext := `
[[site]]
name = "pixeldrain"
cdn_patterns = ["cdn-b"]
cdn_patterns_remove = ["CDN-A"]
`
	cfgs, _, err := Load([]byte(baseTOML), writeTemp(t, ext))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, c := range cfgs[0].CDNPatterns {
		if strings.EqualFold(c, "cdn-a") {
			t.Fatalf("cdn_patterns_remove uygulanmadı: %v", cfgs[0].CDNPatterns)
		}
	}
	if len(cfgs[0].CDNPatterns) != 1 || cfgs[0].CDNPatterns[0] != "cdn-b" {
		t.Fatalf("cdn_patterns = %v, sadece cdn-b kalmalıydı", cfgs[0].CDNPatterns)
	}
}
