package main

import (
	"testing"

	"fyne.io/fyne/v2"
)

// In a window that isn't maximized the Settings dialog grew past the
// window: its one-line hints made it 900 pixels wide, its rows didn't fit,
// and the last ones lay under the Close button. The settings now wrap to
// the dialog's width and scroll, and the dialog stays inside the window.
func TestSettingsFitAWindowThatIsNotMaximized(t *testing.T) {
	for _, lang := range spokenLanguages() {
		old := current
		current = lang
		q, _, _ := newTestQueue(t, "a.bin")
		current = old
		if w := q.settings.MinSize().Width; w > settingsWidth {
			t.Errorf("%s: the settings need %.0f pixels of width, more than the dialog's %d", lang, w, settingsWidth)
		}
	}
	for _, win := range []fyne.Size{{Width: 800, Height: 600}, {Width: 980, Height: 720}, {Width: 1920, Height: 1080}} {
		got := settingsSize(win)
		if got.Width > win.Width-dialogMargin || got.Height > win.Height-dialogMargin {
			t.Errorf("in a %v window the dialog is %v: larger than the window", win, got)
		}
		if got.Width < 400 || got.Height < 400 {
			t.Errorf("in a %v window the dialog is only %v", win, got)
		}
	}
}
