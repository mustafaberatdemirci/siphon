package main

// Telling the user a new Siphon is out (internal/update): once a day the
// latest release is asked for, and a newer one is said on the status line,
// with a system notification once per version. Help → Check for updates
// asks now. Nothing is downloaded: the page has the files.

import (
	"context"
	"net/url"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/update"
)

const (
	// prefCheckUpdates: look for a new version once a day. On by default;
	// Settings turns it off.
	prefCheckUpdates = "check_updates"
	// prefUpdateNotified is the version last told with a system notification.
	prefUpdateNotified = "update_notified"
)

const (
	updateFirstAfter = 30 * time.Second // not while the window is opening
	updateEvery      = 24 * time.Hour
)

// latestRelease is update.Latest at GitHub; tests answer instead.
var latestRelease = func(ctx context.Context) (update.Release, error) {
	return update.Latest(ctx, nil, update.LatestURL)
}

// updateLoop looks for a new version now and then, while the setting is on.
// A build without a version ("dev") has nothing to compare.
func (q *queueTab) updateLoop() {
	if !update.Comparable(version) {
		return
	}
	time.Sleep(updateFirstAfter)
	for {
		if q.prefs.BoolWithFallback(prefCheckUpdates, true) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			r, err := latestRelease(ctx)
			cancel()
			if err == nil && update.Newer(version, r.Version) {
				fyne.Do(func() { q.tellUpdate(r) })
			}
		}
		time.Sleep(updateEvery)
	}
}

// tellUpdate says a new version is out: on the status line every time, with
// a system notification the first time for that version. UI thread.
func (q *queueTab) tellUpdate(r update.Release) {
	msg := Tf("Siphon %s is available (you have %s): Help → Check for updates.", r.Version, version)
	q.vm.Notify(msg)
	if q.prefs.String(prefUpdateNotified) != r.Version {
		q.prefs.SetString(prefUpdateNotified, r.Version)
		fyne.CurrentApp().SendNotification(fyne.NewNotification(Tf("Siphon %s is available", r.Version), msg))
	}
}

// checkForUpdates asks now and says what it found.
func (q *queueTab) checkForUpdates() {
	if !update.Comparable(version) {
		showInfo(T("Check for updates"), Tf("This is a development build (%s): there is no version to compare.", version), q.win)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		r, err := latestRelease(ctx)
		cancel()
		fyne.Do(func() { q.showUpdateResult(r, err) })
	}()
}

// showUpdateResult is checkForUpdates' dialog. UI thread.
func (q *queueTab) showUpdateResult(r update.Release, err error) {
	switch {
	case err != nil:
		showInfo(T("Check for updates"), Tf("Could not check for a new version: %s", firstLine(err.Error())), q.win)
	case !update.Newer(version, r.Version):
		showInfo(T("Check for updates"), Tf("You have the latest version (%s).", version), q.win)
	default:
		l := widget.NewLabel(Tf("Siphon %s is available; you have %s. Download it from its page; your queue and settings stay.", r.Version, version))
		l.Wrapping = fyne.TextWrapWord
		content := container.NewVBox(l)
		if u, perr := url.Parse(r.URL); perr == nil {
			content.Add(widget.NewHyperlink(T("Open the release page"), u))
		}
		d := dialog.NewCustom(T("Check for updates"), T("Close"), content, q.win)
		d.Resize(fyne.NewSize(460, 0))
		d.Show()
	}
}
