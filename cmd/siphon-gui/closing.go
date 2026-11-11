package main

import "fyne.io/fyne/v2"

// The window's background loops (the redraw, the clipboard watch, the
// download hours, the update check) hand their work to the UI thread with
// fyne.Do. While Fyne shuts down it runs such work at once, on the loop's
// own goroutine (it has stopped taking it to its thread): a redraw then
// wrote into widgets beside Fyne's own teardown. MEASURED 2026-11-11: 32
// "Error in Fyne call thread" warnings in siphon.log the moment a Siphon
// was quit. Once the app is quitting, the loops stop drawing.

// close marks the app as quitting. Safe to call more than once, from any
// goroutine.
func (q *queueTab) close() { q.closing.Store(true) }

// do runs fn on the UI thread, unless the app is quitting (then not at all).
func (q *queueTab) do(fn func()) {
	if q.closing.Load() {
		return
	}
	fyne.Do(func() {
		if !q.closing.Load() {
			fn()
		}
	})
}
