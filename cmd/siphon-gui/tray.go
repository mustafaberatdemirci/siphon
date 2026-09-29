package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
)

// prefTrayHintShown: the "Siphon keeps running in the notification area"
// explanation has been shown once.
const prefTrayHintShown = "tray_hint_shown"

// tray keeps Siphon running in the notification area when the window is
// closed, the way download managers do: closing the window must not stop the
// downloads. The icon brings the window back (left click); its menu quits.
type tray struct {
	w      fyne.Window
	desk   desktop.App
	prefs  fyne.Preferences
	quit   func()
	normal fyne.Resource
	quota  fyne.Resource
	// quotaShown: the icon currently shows the "waiting for quota" variant.
	quotaShown bool
}

// setupTray puts Siphon in the notification area and makes closing the
// window hide it. Without a notification area (a driver that doesn't
// support one) closing the window quits, as before: hiding a window nobody
// can bring back would leave an invisible process running.
func setupTray(a fyne.App, w fyne.Window, prefs fyne.Preferences, quit func()) *tray {
	desk, ok := a.(desktop.App)
	if !ok {
		w.SetCloseIntercept(quit)
		return nil
	}
	t := &tray{w: w, desk: desk, prefs: prefs, quit: quit, normal: appIcon(), quota: quotaIcon()}
	desk.SetSystemTrayMenu(fyne.NewMenu("Siphon",
		fyne.NewMenuItem("Show Siphon", t.show),
		// Our own Quit, marked IsQuit so Fyne doesn't add its own: Fyne's
		// would exit without waiting for unfinished jobs to be saved.
		&fyne.MenuItem{Label: "Quit", IsQuit: true, Action: quit},
	))
	desk.SetSystemTrayIcon(t.normal)
	desk.SetSystemTrayWindow(w) // left click on the icon shows the window
	// After SetSystemTrayWindow: it installs a plain w.Hide of its own.
	w.SetCloseIntercept(t.hide)
	return t
}

func (t *tray) show() {
	t.w.Show()
	t.w.RequestFocus()
}

// hide is the window's close button. The first time it says where the app
// went: Windows 11 puts a new notification-area icon under the ^ arrow, and
// Windows notifications may be turned off, so without this the app would
// look closed (and a second launch would be the natural next step).
func (t *tray) hide() {
	if t.prefs.Bool(prefTrayHintShown) {
		t.w.Hide()
		return
	}
	t.prefs.SetBool(prefTrayHintShown, true)
	msg := widget.NewLabel("Closing the window doesn't stop Siphon: it keeps running in the notification area " +
		"next to the clock (on Windows 11 it may be under the ^ arrow), and downloads continue.\n\n" +
		"Click its icon to open this window again. To exit, right-click the icon and choose Quit.")
	msg.Wrapping = fyne.TextWrapWord
	d := dialog.NewCustomConfirm("Siphon keeps running", "Hide", "Quit Siphon", msg, func(hide bool) {
		if hide {
			t.w.Hide()
		} else {
			t.quit()
		}
	}, t.w)
	d.Resize(fyne.NewSize(480, 240))
	d.Show()
}

// setQuota switches the notification-area icon while downloads wait for
// quota: with the window hidden it is the one place left to show it. Must
// run on the UI thread.
func (t *tray) setQuota(waiting bool) {
	if t == nil || waiting == t.quotaShown {
		return
	}
	t.quotaShown = waiting
	if waiting {
		t.desk.SetSystemTrayIcon(t.quota)
	} else {
		t.desk.SetSystemTrayIcon(t.normal)
	}
}
