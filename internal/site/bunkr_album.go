package site

// bunkr: reading an album page.

import (
	"encoding/json"
	"errors"
	"html"
	"strconv"
	"strings"
)

// ---------- Album parsing ----------

type bunkrFile struct {
	ID       string
	Name     string
	Slug     string
	Size     int64
	Ext      string
	MimeType string
}

// parseAlbumFiles parses the window.albumFiles array on the album page.
//
// The source is NOT JSON but embedded JavaScript: unquoted keys and
// line-ending commas. gallery-dl reads it by matching exact separators per
// field (" id: " and "size:  ", double space included). A more tolerant path
// was chosen here on purpose: every line is parsed as "key: value,". Exact
// whitespace matching is precisely the first thing to break, and this file
// exists to make that breakage cheap.
func parseAlbumFiles(page string) ([]bunkrFile, error) {
	const marker = "window.albumFiles"
	i := strings.Index(page, marker)
	if i < 0 {
		return nil, errors.New("window.albumFiles not found")
	}
	rest := page[i:]
	open := strings.Index(rest, "[")
	if open < 0 {
		return nil, errors.New("albumFiles array does not open")
	}
	end := strings.Index(rest, "</script>")
	if end < 0 || end < open {
		end = len(rest)
	}
	body := rest[open+1 : end]

	var out []bunkrFile
	for _, chunk := range splitJSObjects(body) {
		fields := parseJSFields(chunk)
		id := fields["id"]
		if id == "" {
			continue
		}
		f := bunkrFile{
			ID:       id,
			Name:     fields["original"],
			Slug:     fields["slug"],
			Ext:      fields["extension"],
			MimeType: fields["type"],
			Size:     -1,
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(fields["size"]), 10, 64); err == nil && n > 0 {
			f.Size = n
		}
		if f.Name == "" {
			// Without a name, slug + extension is a reasonable fallback; better
			// than skipping an unnamed item.
			f.Name = strings.TrimSuffix(f.Slug, ".") + f.Ext
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, errors.New("no items found in albumFiles")
	}
	return out, nil
}

// splitJSObjects separates "{...}," blocks by brace balance.
// It doesn't trust a line-ending pattern ("\n},\n"): if formatting changes,
// that pattern silently produces one giant item.
func splitJSObjects(body string) []string {
	var out []string
	depth, start := 0, -1
	inStr := false
	var quote rune
	esc := false
	for i, r := range body {
		if inStr {
			switch {
			case esc:
				esc = false
			case r == '\\':
				esc = true
			case r == quote:
				inStr = false
			}
			continue
		}
		switch r {
		case '"', '\'':
			inStr, quote = true, r
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			depth--
			if depth == 0 && start >= 0 {
				out = append(out, body[start:i+1])
				start = -1
			}
		}
	}
	return out
}

// parseJSFields turns a JS object block into a "key -> value" map.
func parseJSFields(chunk string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(chunk, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "{")
		line = strings.TrimSuffix(line, "}")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.Trim(strings.TrimSpace(key), `"'`)
		val = strings.TrimSpace(val)
		val = strings.TrimSuffix(val, ",")
		val = strings.TrimSpace(val)
		if key == "" || val == "" {
			continue
		}
		out[key] = unquoteJS(val)
	}
	return out
}

// unquoteJS unquotes a quoted JS value. If there are JSON escapes it
// decodes them, otherwise it settles for trimming the quotes.
func unquoteJS(v string) string {
	if len(v) < 2 {
		return v
	}
	first, last := v[0], v[len(v)-1]
	if first == '"' && last == '"' {
		var s string
		if json.Unmarshal([]byte(v), &s) == nil {
			return s
		}
		return strings.Trim(v, `"`)
	}
	if first == '\'' && last == '\'' {
		inner := v[1 : len(v)-1]
		return strings.ReplaceAll(inner, `\'`, `'`)
	}
	return v
}

// ogTitle returns the page's og:title value. The value is an HTML attribute,
// so entities ("&amp;", "&#39;") are decoded; otherwise they ended up in
// folder and file names as is.
func ogTitle(page string) string {
	return html.UnescapeString(extractBetween(page, `property="og:title" content="`, `"`))
}

// extractBetween returns the first start...end span; "" if not found.
func extractBetween(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		return ""
	}
	return s[:j]
}
