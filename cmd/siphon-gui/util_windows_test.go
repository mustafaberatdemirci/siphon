//go:build windows

package main

import "testing"

// --- Path normalization ---

// MEASURED: the path Fyne's folder picker returns uses FORWARD SLASHES
// ("E:/x"). Go's file calls accept it, explorer.exe doesn't and silently
// opens the Documents folder.
func TestNormalizeDirConvertsPickerPathForExplorer(t *testing.T) {
	if got := normalizeDir("E:/x"); got != `E:\x` {
		t.Fatalf("normalizeDir(\"E:/x\") = %q", got)
	}
}

func TestNormalizeDirStripsTrailingSeparatorAndQuotes(t *testing.T) {
	for in, want := range map[string]string{
		`E:\x\`: `E:\x`, "E:/x/": `E:\x`, `"E:\x"`: `E:\x`, `  "E:\sub folder"  `: `E:\sub folder`,
	} {
		if got := normalizeDir(in); got != want {
			t.Errorf("normalizeDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeDirEmptyAndValid(t *testing.T) {
	for _, in := range []string{"", "   ", `""`} {
		if got := normalizeDir(in); got != "" {
			t.Errorf("normalizeDir(%q) = %q, expected empty", in, got)
		}
	}
	for _, in := range []string{`E:\x`, `C:\Users\me\Downloads\siphon`, `\\server\share\folder`} {
		if got := normalizeDir(in); got != in {
			t.Errorf("a valid path changed: %q -> %q", in, got)
		}
	}
}

// A bare "E:" is a path RELATIVE TO THE DRIVE; it must be interpreted as the root.
func TestNormalizeDirDriveRoot(t *testing.T) {
	if got := normalizeDir("E:/"); got != `E:\` {
		t.Errorf("E:/ -> %q", got)
	}
	if got := normalizeDir("E:"); got != `E:\` {
		t.Errorf("E: -> %q (a drive-relative path!)", got)
	}
}
