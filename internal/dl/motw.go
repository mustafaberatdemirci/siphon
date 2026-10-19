package dl

import (
	"net/url"
	"strings"
)

// webAddress is the page a download came from, for the mark Windows keeps
// on downloaded files: scheme, host and path only. The fragment carries
// mega's key and the query gofile's password; neither belongs in a file's
// metadata. A tool prefix ("yt-dlp:") is dropped. "" if there is no web
// address in source.
func webAddress(source string) string {
	i := strings.Index(source, "https://")
	if j := strings.Index(source, "http://"); j >= 0 && (i < 0 || j < i) {
		i = j
	}
	if i < 0 {
		return ""
	}
	u, err := url.Parse(source[i:])
	if err != nil || u.Host == "" {
		return ""
	}
	u.User, u.RawQuery, u.Fragment, u.RawFragment = nil, "", "", ""
	return u.String()
}
