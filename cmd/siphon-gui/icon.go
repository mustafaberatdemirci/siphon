package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"

	"fyne.io/fyne/v2"
)

// The app icon is drawn here rather than shipped as a file: the exe stays a
// single file, and the notification-area icon needs a second variant anyway.
// Without an icon Fyne puts a "broken image" symbol in the notification area.
// The release packages (the .exe's icon, the macOS app's) use the same
// drawing, written to docs/icon.png; a test keeps that file current.

var (
	iconBlue  = color.NRGBA{R: 0x25, G: 0x63, B: 0xEB, A: 0xFF} // normal
	iconAmber = color.NRGBA{R: 0xD9, G: 0x77, B: 0x06, A: 0xFF} // waiting for quota
)

// appIcon is the normal icon (window, taskbar, notification area).
func appIcon() fyne.Resource { return iconResource("siphon.png", iconBlue) }

// quotaIcon replaces it in the notification area while downloads wait for
// quota: with the window hidden, and Windows notifications possibly turned
// off, the icon is the one place left to show it.
func quotaIcon() fyne.Resource { return iconResource("siphon-quota.png", iconAmber) }

func iconResource(name string, bg color.NRGBA) fyne.Resource {
	var buf bytes.Buffer
	if err := png.Encode(&buf, drawIcon(256, bg)); err != nil {
		return nil
	}
	return fyne.NewStaticResource(name, buf.Bytes())
}

// drawIcon draws a download arrow over a tray line on a rounded square, at
// any size. The shapes are laid out on a 64-unit grid; every pixel averages
// 4×4 samples so the edges stay smooth when the icon is large.
func drawIcon(size int, bg color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const sub = 4
	scale := 64 / float64(size)
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64
			for sy := 0; sy < sub; sy++ {
				for sx := 0; sx < sub; sx++ {
					u := (float64(px) + (float64(sx)+0.5)/sub) * scale
					v := (float64(py) + (float64(sy)+0.5)/sub) * scale
					c, ok := iconColorAt(u, v, bg)
					if !ok {
						continue
					}
					r += float64(c.R)
					g += float64(c.G)
					b += float64(c.B)
					a++
				}
			}
			if a == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{
				R: uint8(r/a + 0.5), G: uint8(g/a + 0.5), B: uint8(b/a + 0.5),
				A: uint8(a/(sub*sub)*255 + 0.5),
			})
		}
	}
	return img
}

// iconColorAt is the icon's color at (u, v) on the 64-unit grid; false
// outside the rounded square.
func iconColorAt(u, v float64, bg color.NRGBA) (color.NRGBA, bool) {
	if !insideRounded(u, v, 64, 12) {
		return color.NRGBA{}, false
	}
	white := color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	switch {
	case u >= 28 && u < 36 && v >= 10 && v < 32: // arrow shaft
		return white, true
	case v >= 30 && v < 46 && u >= 32-(46-v) && u < 32+(46-v): // arrow head
		return white, true
	case u >= 14 && u < 50 && v >= 50 && v < 55: // tray line
		return white, true
	}
	return bg, true
}

// insideRounded reports whether (x, y) lies in a size×size square with
// corners rounded by r.
func insideRounded(x, y, size, r float64) bool {
	cx, cy := x, y
	switch {
	case x < r:
		cx = r
	case x > size-r:
		cx = size - r
	}
	switch {
	case y < r:
		cy = r
	case y > size-r:
		cy = size - r
	}
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}
