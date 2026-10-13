package main

import (
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"
)

// TestSocialPreview draws docs/social-preview.png, the 1280×640 card that
// GitHub, Reddit and chat apps show when the repository's link is shared.
// GitHub takes it from the repository's settings (Social preview), not from
// the repository itself, so after regenerating it has to be uploaded there.
// It is built from docs/icon.png and docs/screenshot.png; skipped unless
// asked for:
//
//	SIPHON_SOCIAL=1 go test ./cmd/siphon-gui -run SocialPreview
func TestSocialPreview(t *testing.T) {
	if os.Getenv("SIPHON_SOCIAL") == "" {
		t.Skip("set SIPHON_SOCIAL=1 to regenerate docs/social-preview.png")
	}
	test.NewTempApp(t)
	test.ApplyTheme(t, theme.DefaultTheme())
	docs := filepath.Join("..", "..", "docs")
	load := func(name string) *canvas.Image {
		img := canvas.NewImageFromFile(filepath.Join(docs, name))
		if img.File == "" {
			t.Fatalf("no %s", name)
		}
		return img
	}

	const w, h = 1280, 640
	bg := canvas.NewLinearGradient(color.NRGBA{R: 0x10, G: 0x13, B: 0x1a, A: 0xff},
		color.NRGBA{R: 0x16, G: 0x26, B: 0x4a, A: 0xff}, 135)

	text := func(s string, size float32, c color.Color, bold bool) *canvas.Text {
		t := canvas.NewText(s, c)
		t.TextSize = size
		t.TextStyle = fyne.TextStyle{Bold: bold}
		return t
	}
	white := color.NRGBA{R: 0xf3, G: 0xf3, B: 0xf3, A: 0xff}
	soft := color.NRGBA{R: 0xb8, G: 0xc2, B: 0xd6, A: 0xff}
	accent := color.NRGBA{R: 0x6c, G: 0xa6, B: 0xff, A: 0xff}

	place := func(o fyne.CanvasObject, x, y, ow, oh float32) fyne.CanvasObject {
		o.Move(fyne.NewPos(x, y))
		o.Resize(fyne.NewSize(ow, oh))
		return o
	}
	sized := func(t *canvas.Text, x, y float32) fyne.CanvasObject {
		return place(t, x, y, t.MinSize().Width, t.MinSize().Height)
	}

	icon := load("icon.png")
	shot := load("screenshot.png")
	shot.FillMode = canvas.ImageFillStretch
	shot.ScaleMode = canvas.ImageScaleSmooth
	// The window, 1100×640, at 0.72 and bleeding off the right edge.
	const sw, sh = 1100 * 0.72, 640 * 0.72
	frame := canvas.NewRectangle(color.Transparent)
	frame.StrokeColor = color.NRGBA{R: 0x3a, G: 0x4a, B: 0x6e, A: 0xff}
	frame.StrokeWidth = 2
	frame.CornerRadius = 10

	const left = 64
	content := container.NewWithoutLayout(
		place(bg, 0, 0, w, h),
		place(shot, 650, 170, sw, sh),
		place(frame, 649, 169, sw+2, sh+2),
		place(icon, left, 64, 96, 96),
		sized(text("Siphon", 72, white, true), left+116, 66),
		sized(text("One download queue for mega, gofile,", 26, white, false), left, 200),
		sized(text("mediafire, pixeldrain, bunkr and cyberdrop", 26, white, false), left, 236),
		sized(text("Resumable, 8 connections per file", 21, soft, false), left, 318),
		sized(text("Waits out mega's quota, resumes on a VPN switch", 21, soft, false), left, 352),
		sized(text("A window and a CLI, in a single file", 21, soft, false), left, 386),
		sized(text("Windows · macOS · Linux  —  open source, MIT", 20, accent, true), left, 560),
	)
	win := test.NewTempWindow(t, content)
	win.SetPadded(false)
	win.Resize(fyne.NewSize(w, h))

	img := win.Canvas().Capture()
	if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("captured %v, want %dx%d", b, w, h)
	}
	out := filepath.Join(docs, "social-preview.png")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", out)
}
