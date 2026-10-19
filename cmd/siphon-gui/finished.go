package main

// A system notification when the queue runs out of work: "Downloaded: 12
// (3.4 GB). Failed: 1." The window is usually hidden in the notification
// area while a long album downloads; without it the user had to open it to
// see whether everything came.

import "fyne.io/fyne/v2"

// prefNotifyDone: tell when the downloads finish. On by default; Settings
// turns it off.
const prefNotifyDone = "notify_done"

// batchMessage is the notification's text.
func batchMessage(b finishedBatch) string {
	switch {
	case b.failed == 0:
		return Tf("Downloaded: %d (%s).", b.done, humanBytes(b.bytes))
	case b.done == 0:
		return Tf("Failed: %d.", b.failed)
	}
	return Tf("Downloaded: %d (%s). Failed: %d.", b.done, humanBytes(b.bytes), b.failed)
}

// checkFinished tells the user when the queue has run out of work. The
// refresh loop calls it; the notification is sent on the UI thread.
func (q *queueTab) checkFinished() {
	b, ok := q.vm.TakeFinishedBatch()
	if !ok {
		return
	}
	fyne.Do(func() {
		if q.prefs.BoolWithFallback(prefNotifyDone, true) {
			fyne.CurrentApp().SendNotification(fyne.NewNotification(T("Siphon — downloads finished"), batchMessage(b)))
		}
		if q.whenDone != doneNothing {
			q.startWhenDone()
		}
	})
}
