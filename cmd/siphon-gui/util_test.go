package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- Yol normalizasyonu ---

// OLCULDU: Fyne'in klasor seciciden dondurdugu yol EGIK CIZGILI ("E:/x").
// Go'nun dosya cagrilari bunu kabul ediyor, explorer.exe etmiyor ve sessizce
// Belgeler klasorunu aciyor.
func TestNormalizeDirConvertsPickerPathForExplorer(t *testing.T) {
	if got := normalizeDir("E:/x"); got != `E:\x` {
		t.Fatalf("normalizeDir(\"E:/x\") = %q", got)
	}
}

func TestNormalizeDirStripsTrailingSeparatorAndQuotes(t *testing.T) {
	for in, want := range map[string]string{
		`E:\x\`: `E:\x`, "E:/x/": `E:\x`, `"E:\x"`: `E:\x`, `  "E:\alt klasor"  `: `E:\alt klasor`,
	} {
		if got := normalizeDir(in); got != want {
			t.Errorf("normalizeDir(%q) = %q, %q bekleniyordu", in, got, want)
		}
	}
}

func TestNormalizeDirEmptyAndValid(t *testing.T) {
	for _, in := range []string{"", "   ", `""`} {
		if got := normalizeDir(in); got != "" {
			t.Errorf("normalizeDir(%q) = %q, bos bekleniyordu", in, got)
		}
	}
	for _, in := range []string{`E:\x`, `C:\Users\musta\Downloads\siphon`, `\\sunucu\pay\klasor`} {
		if got := normalizeDir(in); got != in {
			t.Errorf("gecerli yol degisti: %q -> %q", in, got)
		}
	}
}

// Ciplak "E:" SURUCUYE GORELI bir yoldur; kok olarak yorumlanmali.
func TestNormalizeDirDriveRoot(t *testing.T) {
	if got := normalizeDir("E:/"); got != `E:\` {
		t.Errorf("E:/ -> %q", got)
	}
	if got := normalizeDir("E:"); got != `E:\` {
		t.Errorf("E: -> %q (surucuye goreli yol!)", got)
	}
}

// --- "Klasoru ac" hedefi ---

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
		t.Errorf("son dosya varken hedef = %q secili=%v", target, sel)
	}
	if target, sel, _ := pickOpenTarget(filepath.Join(album, "yok.mp4"), album, root); target != album || sel {
		t.Errorf("dosya yokken album bekleniyordu: %q %v", target, sel)
	}
	if target, sel, _ := pickOpenTarget("", "", root); target != root || sel {
		t.Errorf("kok bekleniyordu: %q %v", target, sel)
	}
}

// Kok yoksa SESSIZCE OLUSTURULMAMALI; sebep soylenmeli.
func TestPickOpenTargetMissingRootExplains(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "henuz-yok")
	target, _, reason := pickOpenTarget("", "", missing)
	if target != "" || !strings.Contains(reason, "henüz yok") {
		t.Errorf("hedef=%q sebep=%q", target, reason)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("klasor sessizce olusturuldu")
	}
}

// --- Link ayristirma ---

func TestParseLinksSkipsBlankAndComments(t *testing.T) {
	got := parseLinks("https://a\r\n\n# yorum\n  https://b  \n")
	if len(got) != 2 || got[0] != "https://a" || got[1] != "https://b" {
		t.Errorf("parseLinks = %v", got)
	}
}

// --- Hiz olcumu ---

func TestSpeedoNeedsTwoSamplesAndHandlesReset(t *testing.T) {
	var sp speedo
	base := time.Now()
	if r := sp.update(0, base); r != 0 {
		t.Errorf("ilk ornek hiz uretti: %v", r)
	}
	r := sp.update(1<<20, base.Add(time.Second))
	if r < float64(1<<20)*0.9 || r > float64(1<<20)*1.1 {
		t.Errorf("hiz = %v, ~1 MB/s bekleniyordu", r)
	}
	// Sayac geriye giderse negatif hiz olmamali.
	if r := sp.update(0, base.Add(2*time.Second)); r < 0 {
		t.Errorf("negatif hiz: %v", r)
	}
}

func TestHumanRateAndETA(t *testing.T) {
	if humanRate(0) != "" || humanRate(-5) != "" {
		t.Error("bilinmeyen hiz icin bos bekleniyordu")
	}
	if got := humanRate(1 << 20); got != "1.0 MB/s" {
		t.Errorf("humanRate = %q", got)
	}
	if humanETA(0, 100) != "" || humanETA(100, 0) != "" {
		t.Error("kalan/hiz bilinmiyorken ETA basildi")
	}
	if got := humanETA(300, 1); !strings.Contains(got, "dk") {
		t.Errorf("dakika bekleniyordu: %q", got)
	}
	if got := humanETA(1<<40, 1); got != "" {
		t.Errorf("bir gunden uzun tahmin basildi: %q", got)
	}
}
