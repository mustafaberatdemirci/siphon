package dl

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// MaxComponentUTF16 is the maximum number of UTF-16 code units a path
// COMPONENT (a folder name or a file name) can carry.
//
// Windows has two separate length limits and people mix them up:
//
//	MAX_PATH (260)  — the length of the WHOLE path. Go's os package applies the
//	                  `\\?\` conversion for long ABSOLUTE paths itself, so this
//	                  limit is largely solved.
//	255             — NTFS's PER-COMPONENT limit, in UTF-16 code units. The
//	                  `\\?\` prefix does NOT lift it; nothing does.
//
// So truncation has to happen at the component level: the album folder name
// and the file name are each fitted into this limit separately.
//
// We do NOT add the `\\?\` prefix by hand. Adding it turns off path
// normalization: forward slashes and `.`/`..` components are no longer
// resolved. Trusting the os package is right.
const MaxComponentUTF16 = 255

// truncHashLen is the number of hex characters in the truncation suffix.
// Truncation must be deterministic: the same long name must turn into the
// same short name on every run, otherwise resume and the "already exists"
// check stop working.
const truncHashLen = 8

// maxExtUTF16 is the upper bound for the extension kept when truncating. A
// pathological "extension" (300 characters after the dot) must not eat the
// whole budget.
const maxExtUTF16 = 24

// forbidden are the characters that cannot be used in a file name on Windows.
// Control characters (0-31) are handled separately.
const forbidden = `<>:"/\|?*`

// reserved are Windows device names. Opening a file with these names opens a
// DEVICE, not a file; `CON.txt` included, because matching is done on the part
// before the first dot, case-insensitively.
//
// The superscript variants (COM¹, LPT²) are listed separately because some
// code pages resolve them to normal digits.
//
// COM0 and LPT0 are DELIBERATELY missing: they are not classically reserved,
// and a needless rename means corrupting a real file name.
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

// Component makes a single path component safe on Windows and fits it into
// the full 255 UTF-16 unit limit. This is the right one for folder names.
//
// For FILE names ComponentLimit must be used: the downloader appends `.part`
// and `.part.state` to the final name, so spending 255 on the final name
// pushes the component to 266 units and NTFS rejects it.
func Component(name string) string {
	return ComponentLimit(name, MaxComponentUTF16)
}

// ComponentLimit is the same as Component but the caller sets the length
// limit. The caller must reserve room for the suffixes it will append.
//
// If nothing usable is left it returns ""; the decision is the caller's.
func ComponentLimit(name string, limit int) string {
	s := replaceForbidden(name)

	// Trailing dots and spaces: Windows drops them SILENTLY. If we don't, we
	// write "file. " and "file" gets created; then Stat("file. ") matches but
	// rename and the "already exists" check behave unexpectedly.
	s = strings.TrimRight(s, ". ")
	s = strings.TrimLeft(s, " ")

	if s == "" || s == "." || s == ".." {
		return ""
	}

	s = escapeReserved(s)
	s = truncateUTF16(s, name, limit)
	// Re-check after truncation: in an edge case no trailing space/dot may remain.
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
			// Control characters are dropped entirely; a "-" would only add noise.
		case strings.ContainsRune(forbidden, r):
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeReserved prefixes reserved device names with an underscore.
// Matching is done on the part BEFORE the first dot: "CON.txt" is reserved too.
func escapeReserved(s string) string {
	stem := s
	if i := strings.IndexByte(s, '.'); i >= 0 {
		stem = s[:i]
	}
	// Windows device name resolution ignores trailing spaces.
	stem = strings.TrimRight(stem, " ")
	if reserved[strings.ToLower(stem)] {
		return "_" + s
	}
	return s
}

// utf16Len returns the length of a string in UTF-16 code units.
// Runes outside the BMP (like emoji) count as TWO units as a surrogate pair;
// the NTFS limit works in code units, not runes.
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

// truncPrefix truncates a string to at most limit UTF-16 units.
// It cuts on a rune boundary, so a surrogate pair is never split.
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

// truncateUTF16 fits a component into the given number of UTF-16 units.
//
// Truncation uses a hash suffix: two long names in the same folder whose
// first 200 characters are identical would truncate to the SAME name and one
// would overwrite the other. Deriving the suffix from the sha256 of the
// original name prevents that and keeps it deterministic, so resume and the
// "already exists" check keep working across runs.
//
// original is the source of the hash: the RAW name is used, not the sanitized
// one, so the same source name produces the same hash even if the sanitizing
// rules change.
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
	// Truncation may have left a trailing space or dot.
	stem = strings.TrimRight(stem, ". ")

	return stem + suffix + ext
}
