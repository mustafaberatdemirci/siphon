package main

import (
	"time"

	"fyne.io/fyne/v2"

	"github.com/mustafaberatdemirci/siphon/internal/browser"
)

// prefWatchClipboard: links of supported sites copied anywhere open the Add
// dialog. On by default; Settings turns it off.
const prefWatchClipboard = "watch_clipboard"

// clipboardEvery is how often the clipboard is looked at.
const clipboardEvery = time.Second

// maxClipboardText: a larger clipboard (a copied document) isn't searched.
const maxClipboardText = 1 << 20

// clipboardLoop looks at the clipboard every second, on the UI thread.
func (q *queueTab) clipboardLoop() {
	t := time.NewTicker(clipboardEvery)
	defer t.Stop()
	for range t.C {
		fyne.Do(func() { q.checkClipboard(fyne.CurrentApp().Clipboard().Content()) })
	}
}

// checkClipboard offers the links of supported sites in a new clipboard text
// through the Add dialog. What was there when Siphon started isn't offered,
// a link is offered once per session, and only real sites' links count: not
// every web address someone copies is a download. UI thread only.
func (q *queueTab) checkClipboard(text string) {
	if !q.clipStarted {
		q.clipStarted, q.clipLast = true, text
		return
	}
	if text == q.clipLast {
		return
	}
	q.clipLast = text
	if !q.prefs.BoolWithFallback(prefWatchClipboard, true) || len(text) > maxClipboardText {
		return
	}
	if q.clipOffered == nil {
		q.clipOffered = map[string]bool{}
	}
	var links []string
	for _, l := range browser.ExtractLinks(text) {
		if q.eng.Recognizes(l) && !q.clipOffered[l] {
			q.clipOffered[l] = true
			links = append(links, l)
		}
	}
	q.receiveLinks(links)
}
