package main

import (
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// iconFile is the app icon as the release packages use it (the Windows
// .exe's, the macOS app's).
var iconFile = filepath.Join("..", "..", "docs", "icon.png")

const iconFileSize = 1024

// The icon file is the drawing in icon.go. After changing the drawing,
// write it again with SIPHON_WRITE_ICON=1 go test ./cmd/siphon-gui -run IconFile
func TestIconFileIsCurrent(t *testing.T) {
	want := drawIcon(iconFileSize, iconBlue)
	if os.Getenv("SIPHON_WRITE_ICON") != "" {
		f, err := os.Create(iconFile)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, want); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", iconFile)
		return
	}
	f, err := os.Open(iconFile)
	if err != nil {
		t.Fatalf("%v; write it with SIPHON_WRITE_ICON=1", err)
	}
	defer f.Close()
	got, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bounds() != want.Bounds() {
		t.Fatalf("%s is %v, want %v; write it again with SIPHON_WRITE_ICON=1", iconFile, got.Bounds(), want.Bounds())
	}
	for y := 0; y < iconFileSize; y++ {
		for x := 0; x < iconFileSize; x++ {
			r1, g1, b1, a1 := got.At(x, y).RGBA()
			r2, g2, b2, a2 := want.At(x, y).RGBA()
			if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
				t.Fatalf("%s differs from the drawing at (%d, %d); write it again with SIPHON_WRITE_ICON=1", iconFile, x, y)
			}
		}
	}
}

// The corners are transparent, the middle is the icon's blue or white, and
// an edge pixel is blended rather than jagged.
func TestDrawIcon(t *testing.T) {
	img := drawIcon(256, iconBlue)
	if a := img.NRGBAAt(0, 0).A; a != 0 {
		t.Errorf("corner alpha %d, want transparent", a)
	}
	// On the 64-unit grid the shaft is at u 28-36, v 10-32; the square's
	// blue fills the rest. At 256 px a unit is 4 px.
	if c := img.NRGBAAt(32*4, 20*4); c != (color.NRGBA{R: 255, G: 255, B: 255, A: 255}) {
		t.Errorf("middle of the arrow shaft: %v, want white", c)
	}
	if c := img.NRGBAAt(10*4, 32*4); c != iconBlue {
		t.Errorf("left of the arrow: %v, want the icon's blue", c)
	}
	partial := 0
	for x := 0; x < 256; x++ {
		if a := img.NRGBAAt(x, 4).A; a > 0 && a < 255 {
			partial++
		}
	}
	if partial == 0 {
		t.Error("no blended pixels along the rounded corner: edges are jagged")
	}
}
