package main

// Tasks → When downloads finish: do nothing, quit Siphon, sleep or shut the
// computer down once nothing is left to download. It isn't saved: every
// start is "do nothing", so a forgotten choice can't shut the computer down
// at the end of a download days later. The action waits a minute behind a
// dialog with Cancel; links added meanwhile cancel it too.

import (
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

type whenDone int

const (
	doneNothing whenDone = iota
	doneQuit
	doneSleep
	doneShutdown
)

// whenDoneCountdown is how long the dialog waits before acting.
var whenDoneCountdown = 60 * time.Second

// powerActions does what the choice says; tests replace them.
var powerActions = map[whenDone]func(q *queueTab) error{
	doneQuit: func(q *queueTab) error {
		if q.quit != nil {
			q.quit()
		}
		return nil
	},
	doneSleep:    func(*queueTab) error { return sleepComputer() },
	doneShutdown: func(*queueTab) error { return shutdownComputer() },
}

func (w whenDone) label() string {
	switch w {
	case doneQuit:
		return T("Quit Siphon")
	case doneSleep:
		return T("Put the computer to sleep")
	case doneShutdown:
		return T("Shut the computer down")
	}
	return T("Do nothing")
}

// countdownText is the dialog's line for the seconds left.
func (w whenDone) countdownText(left int) string {
	switch w {
	case doneQuit:
		return Tf("The downloads are finished. Siphon quits in %d s.", left)
	case doneSleep:
		return Tf("The downloads are finished. The computer goes to sleep in %d s.", left)
	}
	return Tf("The downloads are finished. The computer shuts down in %d s.", left)
}

// whenDoneMenu is the Tasks menu's submenu; the choice is ticked.
func (q *queueTab) whenDoneMenu() *fyne.MenuItem {
	parent := fyne.NewMenuItem(T("When downloads finish"), nil)
	var items []*fyne.MenuItem
	for _, w := range []whenDone{doneNothing, doneQuit, doneSleep, doneShutdown} {
		item := fyne.NewMenuItem(w.label(), func() {
			q.setWhenDone(w)
			if w != doneNothing {
				q.vm.Notify(Tf("When the downloads finish: %s.", w.label()))
			}
		})
		item.Checked = q.whenDone == w
		items = append(items, item)
	}
	parent.ChildMenu = fyne.NewMenu("", items...)
	q.whenDoneItems = items
	return parent
}

// setWhenDone changes the choice and the menu's tick. UI thread.
func (q *queueTab) setWhenDone(w whenDone) {
	q.whenDone = w
	for i, it := range q.whenDoneItems {
		it.Checked = whenDone(i) == w
	}
	if mm := q.win.MainMenu(); mm != nil {
		mm.Refresh()
	}
}

// startWhenDone shows the countdown and then does what was chosen. UI thread.
func (q *queueTab) startWhenDone() {
	w := q.whenDone
	act := powerActions[w]
	if act == nil || q.countdown != nil {
		return
	}
	left := max(1, int(whenDoneCountdown/time.Second))
	text := widget.NewLabel(w.countdownText(left))
	stop := make(chan struct{})
	d := dialog.NewCustom(T("When downloads finish"), T("Cancel"), text, q.win)
	q.countdown = d
	d.SetOnClosed(func() {
		select {
		case <-stop:
		default:
			close(stop) // Cancel: the choice goes back to "do nothing"
			q.setWhenDone(doneNothing)
			q.vm.Notify(T("Canceled; when the downloads finish, nothing is done."))
		}
		q.countdown = nil
	})
	d.Show()
	q.win.Show()
	q.win.RequestFocus()

	step := whenDoneCountdown / time.Duration(left)
	go func() {
		t := time.NewTicker(step)
		defer t.Stop()
		for left > 0 {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			left--
			fyne.Do(func() {
				select {
				case <-stop:
					return
				default:
				}
				if q.vm.Busy() {
					// Links were added: not finished after all. The choice
					// stays, for when these are done.
					close(stop)
					d.Hide()
					return
				}
				text.SetText(w.countdownText(left))
				if left > 0 {
					return
				}
				close(stop)
				d.Hide()
				q.setWhenDone(doneNothing)
				if err := act(q); err != nil {
					q.vm.Notify(Tf("%s: it didn't work (%v)", w.label(), err))
				}
			})
		}
	}()
}
