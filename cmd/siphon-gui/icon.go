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

var (
	iconBlue  = color.NRGBA{R: 0x25, G: 0x63, B: 0xEB, A: 0xFF} // normal
	iconAmber = color.NRGBA{R: 0xD9, G: 0x77, B: 0x06, A: 0xFF} // waiting for quota
)

// appIcon is the normal icon (window, taskbar, notification area).
func appIcon() fyne.Resource { return drawIcon("siphon.png", iconBlue) }

// quotaIcon replaces it in the notification area while downloads wait for
// quota: with the window hidden, and Windows notifications possibly turned
// off, the icon is the one place left to show it.
func quotaIcon() fyne.Resource { return drawIcon("siphon-quota.png", iconAmber) }

// drawIcon draws a download arrow over a tray line on a rounded square.
func drawIcon(name string, bg color.NRGBA) fyne.Resource {
	const size = 64
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	white := color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	const r = 12 // corner radius
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if insideRounded(x, y, size, r) {
				img.SetNRGBA(x, y, bg)
			}
		}
	}
	fill := func(x0, y0, x1, y1 int) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img.SetNRGBA(x, y, white)
			}
		}
	}
	fill(28, 10, 36, 32) // arrow shaft
	for y := 30; y < 46; y++ {
		half := 46 - y // arrow head narrows to a point
		fill(32-half, y, 32+half, y+1)
	}
	fill(14, 50, 50, 55) // tray line

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return fyne.NewStaticResource(name, buf.Bytes())
}

// insideRounded reports whether (x, y) lies in a size×size square with corners rounded by r.
func insideRounded(x, y, size, r int) bool {
	cx, cy := x, y
	switch {
	case x < r:
		cx = r
	case x >= size-r:
		cx = size - r - 1
	}
	switch {
	case y < r:
		cy = r
	case y >= size-r:
		cy = size - r - 1
	}
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}
