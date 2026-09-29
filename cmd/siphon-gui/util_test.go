package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

// --- "Open folder" target ---

func TestPickOpenTargetOrder(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(album, "video.mp4")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if target, sel, _ := pickOpenTarget(file, album, root); target != file || !sel {
		t.Errorf("with a last file, target = %q selected=%v", target, sel)
	}
	if target, sel, _ := pickOpenTarget(filepath.Join(album, "missing.mp4"), album, root); target != album || sel {
		t.Errorf("without the file the album was expected: %q %v", target, sel)
	}
	if target, sel, _ := pickOpenTarget("", "", root); target != root || sel {
		t.Errorf("the root was expected: %q %v", target, sel)
	}
}

// If the root is missing it must NOT BE CREATED SILENTLY; the reason must be given.
func TestPickOpenTargetMissingRootExplains(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-yet")
	target, _, reason := pickOpenTarget("", "", missing)
	if target != "" || !strings.Contains(reason, "doesn't exist yet") {
		t.Errorf("target=%q reason=%q", target, reason)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("the folder was created silently")
	}
}

// --- Link parsing ---

func TestParseLinksSkipsBlankAndComments(t *testing.T) {
	got := parseLinks("https://a\r\n\n# comment\n  https://b  \n")
	if len(got) != 2 || got[0] != "https://a" || got[1] != "https://b" {
		t.Errorf("parseLinks = %v", got)
	}
}

// --- Speed measurement ---

func TestSpeedoNeedsTwoSamplesAndHandlesReset(t *testing.T) {
	var sp speedo
	base := time.Now()
	if r := sp.update(0, base); r != 0 {
		t.Errorf("the first sample produced a speed: %v", r)
	}
	r := sp.update(1<<20, base.Add(time.Second))
	if r < float64(1<<20)*0.9 || r > float64(1<<20)*1.1 {
		t.Errorf("speed = %v, ~1 MB/s expected", r)
	}
	// If the counter goes backwards there must be no negative speed.
	if r := sp.update(0, base.Add(2*time.Second)); r < 0 {
		t.Errorf("negative speed: %v", r)
	}
}

func TestHumanRateAndETA(t *testing.T) {
	if humanRate(0) != "" || humanRate(-5) != "" {
		t.Error("expected empty for an unknown speed")
	}
	if got := humanRate(1 << 20); got != "1.0 MB/s" {
		t.Errorf("humanRate = %q", got)
	}
	if humanETA(0, 100) != "" || humanETA(100, 0) != "" {
		t.Error("an ETA was printed with an unknown remainder/speed")
	}
	if got := humanETA(300, 1); got != "5m" {
		t.Errorf("minutes expected: %q", got)
	}
	if got := humanETA(1<<40, 1); got != "" {
		t.Errorf("an estimate longer than a day was printed: %q", got)
	}
}
