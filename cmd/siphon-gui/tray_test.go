package main

import (
	"bytes"
	"image/png"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
)

// The icons are real 64x64 PNGs with transparent rounded corners, and the
// quota variant differs from the normal one.
func TestIconsDecode(t *testing.T) {
	normal, quota := appIcon(), quotaIcon()
	for _, r := range []fyne.Resource{normal, quota} {
		img, err := png.Decode(bytes.NewReader(r.Content()))
		if err != nil {
			t.Fatalf("%s: %v", r.Name(), err)
		}
		if b := img.Bounds(); b.Dx() != 64 || b.Dy() != 64 {
			t.Errorf("%s is %dx%d", r.Name(), b.Dx(), b.Dy())
		}
		if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
			t.Errorf("%s: the corner isn't transparent", r.Name())
		}
		if _, _, _, a := img.At(32, 32).RGBA(); a == 0 {
			t.Errorf("%s: the middle is empty", r.Name())
		}
	}
	if bytes.Equal(normal.Content(), quota.Content()) {
		t.Error("the quota icon is the same as the normal one")
	}
}

// fakeDesk records what the tray sets.
type fakeDesk struct{ icons []string }

func (f *fakeDesk) SetSystemTrayMenu(*fyne.Menu)      {}
func (f *fakeDesk) SetSystemTrayIcon(r fyne.Resource) { f.icons = append(f.icons, r.Name()) }
func (f *fakeDesk) SetSystemTrayWindow(fyne.Window)   {}

// The notification-area icon switches while downloads wait for quota, and
// only when the state changes. A nil tray (no notification area) is fine.
func TestTrayQuotaIcon(t *testing.T) {
	desk := &fakeDesk{}
	tr := &tray{desk: desk, normal: appIcon(), quota: quotaIcon()}
	tr.setQuota(false)
	tr.setQuota(true)
	tr.setQuota(true)
	tr.setQuota(false)
	if got := desk.icons; len(got) != 2 || got[0] != "siphon-quota.png" || got[1] != "siphon.png" {
		t.Errorf("icons set: %v; want the quota icon once, then the normal one", got)
	}
	var none *tray
	none.setQuota(true) // must not panic
}

// The close button hides the window. The first time it explains where the
// app went (the window stays until the user answers); after that it hides
// straight away.
func TestTrayCloseHides(t *testing.T) {
	a := test.NewTempApp(t)
	w := a.NewWindow("Siphon")
	w.Show()
	quit := 0
	tr := &tray{w: w, prefs: a.Preferences(), quit: func() { quit++ }}

	tr.hide()
	if !a.Preferences().Bool(prefTrayHintShown) {
		t.Error("the first close didn't record that the explanation was shown")
	}
	tr.hide()
	if quit != 0 {
		t.Error("closing the window quit the app")
	}
}
