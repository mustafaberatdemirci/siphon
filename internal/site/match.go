package site

import "strings"

// MatchHost, bir host'un desen listesinden herhangi birine uyup uymadığını söyler.
//
// Desenler ya birebir bir host ("pixeldrain.com") ya da tek yıldızlı bir joker
// olabilir ("bunkr.*", "*.bunkr.la"). Joker boş olmayan bir dizi karşılar ve
// etiket sınırına saygı duyar: yıldız bir etiketin ortasına taşamaz. Yani
// "bunkr.*" için "bunkr.cr" ve "bunkr.co.uk" eşleşir, "notbunkr.cr" eşleşmez,
// "bunkr." de eşleşmez.
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
	// Nokta sınırına saygı: prefix nokta ile bitmiyorsa joker etiketin ortasına
	// taşamaz. Böylece "bunkr.*" için "notbunkr.cr" elenir.
	if prefix != "" && !strings.HasSuffix(prefix, ".") && strings.Contains(middle, ".") {
		return false
	}
	return true
}
