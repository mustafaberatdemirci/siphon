package dl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComponentForbiddenCharacters(t *testing.T) {
	cases := []struct{ in, want string }{
		{`a<b>c:d"e|f?g*h`, "a-b-c-d-e-f-g-h"},
		{`path/separator`, "path-separator"},
		{`back\slash`, "back-slash"},
		// Control characters are dropped entirely; a "-" would only add noise.
		{"tab\tand\nnewline", "tabandnewline"},
		{"del\x7fcharacter", "delcharacter"},
		// Normal characters are untouched; non-ASCII and spaces are kept.
		{"Holiday 2026 - Zoë & Chloé.mp4", "Holiday 2026 - Zoë & Chloé.mp4"},
	}
	for _, c := range cases {
		if got := Component(c.in); got != c.want {
			t.Errorf("Component(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Windows SILENTLY drops trailing dots and spaces. If we don't, the name
// created on disk differs from the name we think we have, and the "already
// exists" check and rename behave unexpectedly.
func TestComponentTrailingDotsAndSpaces(t *testing.T) {
	cases := []struct{ in, want string }{
		{"file.", "file"},
		{"file...", "file"},
		{"file ", "file"},
		{"file. . ", "file"},
		{"  leading space", "leading space"},
		{"file.txt", "file.txt"},
	}
	for _, c := range cases {
		if got := Component(c.in); got != c.want {
			t.Errorf("Component(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestComponentEmptyAndDotNames(t *testing.T) {
	for _, in := range []string{"", "   ", ".", "..", "...", ". . .", "\x00\x01"} {
		if got := Component(in); got != "" {
			t.Errorf("Component(%q) = %q, expected empty", in, got)
		}
	}
}

// Opening a file with a reserved device name opens a DEVICE, not a file.
// Matching is on the part before the first dot, case-insensitive.
func TestComponentReservedDeviceNames(t *testing.T) {
	reservedInputs := []string{
		"CON", "con", "Con", "PRN", "AUX", "NUL",
		"COM1", "com9", "LPT1", "lpt9",
		"CONIN$", "conout$",
		"COM¹", "LPT³",
		// With an extension it is reserved too: CON.txt also resolves to the device.
		"CON.txt", "nul.mp4", "com1.tar.gz",
		// Device name resolution ignores trailing spaces.
		"CON .txt",
	}
	for _, in := range reservedInputs {
		got := Component(in)
		if !strings.HasPrefix(got, "_") {
			t.Errorf("Component(%q) = %q, reserved name was not escaped", in, got)
		}
	}

	// Names that are NOT reserved but look alike must be left untouched.
	safe := []string{
		"CONSOLE", "console.txt", "COM", "COM10", "COM0", "LPT0",
		"NULL", "nullable.json", "printer.txt", "auxiliary",
		"my CON file.txt", // the part before the first dot is "my CON file"
	}
	for _, in := range safe {
		if got := Component(in); got != in {
			t.Errorf("Component(%q) = %q, should not have changed", in, got)
		}
	}
}

func TestUTF16Len(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"Chloé", 5},      // inside the BMP, each rune is 1 unit
		{"日本語", 3},        // inside the BMP
		{"\U0001F600", 2}, // emoji: outside the BMP, surrogate pair = 2 units
		{"a\U0001F600b", 4},
	}
	for _, c := range cases {
		if got := utf16Len(c.in); got != c.want {
			t.Errorf("utf16Len(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestComponentTruncatesToUTF16Limit(t *testing.T) {
	long := strings.Repeat("a", 400) + ".mp4"
	got := Component(long)
	if n := utf16Len(got); n > MaxComponentUTF16 {
		t.Fatalf("length %d, should be at most %d", n, MaxComponentUTF16)
	}
	if filepath.Ext(got) != ".mp4" {
		t.Errorf("extension not kept: %q", got)
	}
	if !strings.Contains(got, "~") {
		t.Errorf("no truncation suffix: %q", got)
	}
}

// Truncation MUST be deterministic: otherwise resume and the "already exists"
// check don't work across runs.
func TestTruncationIsDeterministic(t *testing.T) {
	long := strings.Repeat("b", 500) + ".bin"
	a := Component(long)
	b := Component(long)
	if a != b {
		t.Fatalf("the same input produced two different outputs:\n%q\n%q", a, b)
	}
}

// If two long names whose first 255 characters are IDENTICAL truncate to the
// same name, one overwrites the other. The hash suffix exists to prevent that.
func TestTruncationAvoidsCollisionOnSharedPrefix(t *testing.T) {
	prefix := strings.Repeat("c", 300)
	a := Component(prefix + "-first.mp4")
	b := Component(prefix + "-second.mp4")
	if a == b {
		t.Fatalf("two different long names truncated to the same short name: %q", a)
	}
	if utf16Len(a) > MaxComponentUTF16 || utf16Len(b) > MaxComponentUTF16 {
		t.Fatal("truncation did not respect the limit")
	}
}

// A long name with emoji: a surrogate pair must not be split and the limit must hold.
func TestTruncationDoesNotSplitSurrogatePairs(t *testing.T) {
	long := strings.Repeat("\U0001F600", 300) + ".png"
	got := Component(long)
	if n := utf16Len(got); n > MaxComponentUTF16 {
		t.Fatalf("length %d, limit %d", n, MaxComponentUTF16)
	}
	if !utf8Valid(got) {
		t.Fatalf("invalid UTF-8 produced: %q", got)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

// A pathological "extension" (very long after the dot) must not eat the whole budget.
func TestPathologicalExtensionIsDropped(t *testing.T) {
	long := strings.Repeat("d", 100) + "." + strings.Repeat("e", 300)
	got := Component(long)
	if n := utf16Len(got); n > MaxComponentUTF16 {
		t.Fatalf("length %d, limit %d", n, MaxComponentUTF16)
	}
}

// A name that fits the limit exactly must not be truncated.
func TestExactLimitIsNotTruncated(t *testing.T) {
	exact := strings.Repeat("f", MaxComponentUTF16)
	got := Component(exact)
	if got != exact {
		t.Fatalf("a name exactly at the limit was truncated: %d -> %d units", MaxComponentUTF16, utf16Len(got))
	}
}

// The shape of a name seen in a real album: long, with special characters
// and parentheses, but valid as is.
func TestRealWorldAlbumFilename(t *testing.T) {
	in := "2024-01-02 - Summer Trip - Lake District - Day by the Water (also known as Hiking Diary - Part One with.mp4"
	got := Component(in)
	if got != in {
		t.Errorf("a valid name was changed:\n%q\n%q", in, got)
	}
}

// The album folder name goes through the SAME sanitizer; there must be no
// separate code path.
func TestComponentUsedForDirectoryLabels(t *testing.T) {
	if got := Component("Album: 2026 / Summer"); got != "Album- 2026 - Summer" {
		t.Errorf("folder name = %q", got)
	}
	if got := Component("NUL"); got != "_NUL" {
		t.Errorf("reserved folder name = %q", got)
	}
}

// Integration: does the sanitizer really reach the disk? The unit tests prove
// Component but not that Download calls it. If sanitizing lived in the
// caller, a caller could skip it; this test pins that connection.
func TestDownloadAppliesComponentSanitizer(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)

	cases := []struct {
		rawName, rawDir string
		wantName        string
		wantDir         string
	}{
		// Reserved device name: a DEVICE would be opened, not a file.
		{"CON.txt", "", "_CON.txt", ""},
		// Forbidden characters and a trailing dot.
		{`bad:name?.mp4`, "", "bad-name-.mp4", ""},
		{"trailing dot.mp4.", "", "trailing dot.mp4", ""},
		// The folder name goes through the same sanitizer; "/" separates
		// nested levels, each sanitized on its own.
		{"ok.bin", "Album: Summer / 2026", "ok.bin", filepath.Join("Album- Summer", "2026")},
	}

	for _, c := range cases {
		d := &Downloader{Client: srv.Client()}
		it := testItem(srv.URL+"/data.bin", c.rawName)
		it.Dir = c.rawDir
		it.SHA256 = payloadSHA()
		if _, err := d.Download(context.Background(), out, it); err != nil {
			t.Fatalf("Download(%q): %v", c.rawName, err)
		}
		want := filepath.Join(out, c.wantDir, c.wantName)
		if _, err := os.Stat(want); err != nil {
			t.Errorf("expected path missing: %s (%v)", want, err)
		}
	}

	// A name beyond the 255-unit limit must really land on disk shortened.
	d := &Downloader{Client: srv.Client()}
	long := strings.Repeat("z", 400) + ".mp4"
	it := testItem(srv.URL+"/data.bin", long)
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("long name: %v", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "zzz") {
			found = true
			// THE REAL INVARIANT: the final name + ".part.state" must also fit
			// the component limit. This integration test found exactly this
			// bug: the sanitizer spent 255 on the final name and .part.state
			// reached 266 units and was rejected by NTFS.
			if n := utf16Len(e.Name()) + utf16Len(stateSuffix); n > MaxComponentUTF16 {
				t.Errorf("name + %q = %d units, limit %d (%q)",
					stateSuffix, n, MaxComponentUTF16, e.Name())
			}
			if !strings.Contains(e.Name(), "~") {
				t.Errorf("the truncation suffix did not reach the disk: %q", e.Name())
			}
		}
	}
	if !found {
		t.Error("the file with the long name was not found on disk")
	}
}

func TestDirPathKeepsLevelsAndStaysInside(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"Album", "Album"},
		{"Parent/Child", filepath.Join("Parent", "Child")},
		{"/Parent//Child/", filepath.Join("Parent", "Child")},
		{"../../etc", "etc"},
		{"a/./b", filepath.Join("a", "b")},
		{`back\slash`, "back-slash"},
		{"CON/x", filepath.Join("_CON", "x")},
	}
	for _, c := range cases {
		if got := DirPath(c.in); got != c.want {
			t.Errorf("DirPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
