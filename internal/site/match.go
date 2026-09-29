package site

import "strings"

// MatchHost reports whether a host matches any pattern in the list.
//
// A pattern is either an exact host ("pixeldrain.com") or a wildcard with a
// single star ("bunkr.*", "*.bunkr.la"). The wildcard matches EXACTLY ONE
// label: the part it covers can be neither empty nor contain a dot.
//
// The single-label rule is a security boundary, not a convenience. If the
// wildcard could span labels, "bunkr.*" would match "bunkr.attacker.com" and
// a hostile link in the input list would be treated as a trusted site and
// fetched. If a multi-part TLD is needed ("bunkr.co.uk") it is added to the
// list explicitly; without the public suffix list there is no safe way to
// express that with a wildcard.
//
// Wildcard support is deliberate: gallery-dl gave up enumerating bunkr TLDs
// on 2024-08-24 and added a wildcard option, and cyberdrop-dl also uses
// "bunkr.*". Keeping a list for this site is a lost battle.
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
	// A common convenience: drop a scheme or path given in the pattern.
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	// Drop a port if present (bracketed IPv6 is left untouched).
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

	// The wildcard part cannot be empty: "bunkr.*" must not match host "bunkr.".
	if middle == "" {
		return false
	}
	// Exactly one label: no dots. "bunkr.attacker.com" is rejected here.
	if strings.Contains(middle, ".") {
		return false
	}
	return true
}
