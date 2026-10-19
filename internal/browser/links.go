// Package browser connects Siphon to web browsers: the native messaging host
// the Siphon extension talks to, its installation for each browser, and
// finding links in text (the clipboard, a selection).
package browser

import (
	"regexp"
	"strings"
)

// linkPattern finds web links in free text: with a scheme, with a tool prefix
// ("yt-dlp:https://…"), or a bare host of a file site people paste without
// one ("mega.nz/folder/…").
var linkPattern = regexp.MustCompile(`(?i)(?:(?:yt-dlp:|gallery-dl:)?https?://|\b(?:www\.)?(?:mega\.nz|mega\.co\.nz|gofile\.io|mediafire\.com|pixeldrain\.com|cyberfile\.me)/)[^\s<>"'` + "`" + `]+`)

// ExtractLinks returns the links in text, each once, in order. Punctuation
// that ends a sentence or closes brackets around a link isn't part of it.
// Only http(s) links come out: others (ftp:, javascript:) are dropped.
func ExtractLinks(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range linkPattern.FindAllString(text, -1) {
		l := trimLink(m)
		if !strings.Contains(strings.ToLower(l), "://") {
			l = "https://" + l
		}
		if len(l) < 12 || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

// trimLink drops what text puts after a link: sentence punctuation and
// closing brackets that have no opening one inside the link.
func trimLink(l string) string {
	for len(l) > 0 {
		last := l[len(l)-1]
		switch last {
		case '.', ',', ';', ':', '!', '?', '\'', '"':
			l = l[:len(l)-1]
			continue
		case ')', ']', '}':
			open := map[byte]byte{')': '(', ']': '[', '}': '{'}[last]
			if strings.Count(l, string(open)) < strings.Count(l, string(last)) {
				l = l[:len(l)-1]
				continue
			}
		}
		return l
	}
	return l
}
