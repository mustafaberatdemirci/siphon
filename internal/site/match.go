package site

import "strings"

// MatchHost, bir host'un desen listesinden herhangi birine uyup uymadığını söyler.
//
// Desenler ya birebir bir host ("pixeldrain.com") ya da tek yıldızlı bir joker
// olabilir ("bunkr.*", "*.bunkr.la"). Joker TAM OLARAK BİR etiket karşılar:
// karşıladığı kısım boş olamaz ve nokta içeremez.
//
// Tek etiket kuralı bir güvenlik sınırıdır, kolaylık değil. Joker çok etiketli
// olsaydı "bunkr.*" deseni "bunkr.attacker.com" ile eşleşirdi ve girdi
// listesindeki düşmanca bir link güvenilen site sayılıp çekilirdi. Çok parçalı
// bir TLD gerekirse ("bunkr.co.uk") açıkça listeye eklenir; kamu son ek listesi
// olmadan bunu jokerle güvenli biçimde ifade etmenin yolu yok.
//
// Joker desteği kasıtlı: gallery-dl 2024-08-24'te bunkr TLD'lerini saymaktan
// vazgeçip joker seçeneği ekledi, cyberdrop-dl de "bunkr.*" kullanıyor. Liste
// tutmak bu sitede kaybedilmiş bir savaş.
func MatchHost(host string, patterns []string) bool {
	host = normalizeHost(host)
	if host == "" {
		return false
	}
	for _, p := range patterns {
		if hostMatchesPattern(host, normalizeHost(p)) {
			return true
		}
	}
	return false
}

func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	// Yaygın bir yazım kolaylığı: desende şema veya yol verilmişse at.
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	// Port varsa at (IPv6 köşeli parantezli hali dokunulmadan bırakılır).
	if !strings.HasPrefix(h, "[") {
		if i := strings.LastIndex(h, ":"); i >= 0 && strings.Count(h, ":") == 1 {
			h = h[:i]
		}
	}
	return h
}

func hostMatchesPattern(host, pattern string) bool {
	if pattern == "" {
		return false
	}
	star := strings.IndexByte(pattern, '*')
	if star < 0 {
		return host == pattern
	}
	prefix, suffix := pattern[:star], pattern[star+1:]
	if len(host) < len(prefix)+len(suffix) {
		return false
	}
	if !strings.HasPrefix(host, prefix) || !strings.HasSuffix(host, suffix) {
		return false
	}
	middle := host[len(prefix) : len(host)-len(suffix)]

	// Jokerin karşıladığı kısım boş olamaz: "bunkr.*" host "bunkr." ile eşleşmesin.
	if middle == "" {
		return false
	}
	// Tam olarak bir etiket: nokta içeremez. "bunkr.attacker.com" burada elenir.
	if strings.Contains(middle, ".") {
		return false
	}
	return true
}
