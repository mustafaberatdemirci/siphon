package main

import (
	"errors"
	"strings"
	"testing"

	"fyne.io/fyne/v2/test"

	"github.com/mustafaberatdemirci/siphon/internal/update"
)

// A newer version is told on the status line, with a system notification
// only the first time for that version.
func TestTellUpdate(t *testing.T) {
	a := test.NewTempApp(t)
	old := version
	version = "v0.1.2"
	t.Cleanup(func() { version = old })
	q := &queueTab{prefs: a.Preferences(), vm: newViewModel()}

	r := update.Release{Version: "v0.1.3", URL: "https://github.com/mustafaberatdemirci/siphon/releases/tag/v0.1.3"}
	q.tellUpdate(r)
	if got := q.vm.StatusLine(); !strings.Contains(got, "v0.1.3") || !strings.Contains(got, "v0.1.2") {
		t.Errorf("status line: %q", got)
	}
	if got := a.Preferences().String(prefUpdateNotified); got != "v0.1.3" {
		t.Errorf("the notified version is %q, want v0.1.3", got)
	}
}

// The dialog's three answers; none of them may panic or block.
func TestShowUpdateResult(t *testing.T) {
	a := test.NewTempApp(t)
	old := version
	version = "v0.1.2"
	t.Cleanup(func() { version = old })
	w := a.NewWindow("x")
	q := &queueTab{prefs: a.Preferences(), vm: newViewModel(), win: w}
	q.showUpdateResult(update.Release{}, errors.New("offline"))
	q.showUpdateResult(update.Release{Version: "v0.1.2"}, nil)
	q.showUpdateResult(update.Release{Version: "v0.1.3", URL: "https://github.com/x"}, nil)
}
