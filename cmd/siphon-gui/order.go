package main

// Changing the queue's order from the window (the engine's MoveToTop,
// MoveToBottom, Arrange): the selected rows go to the top or the bottom of
// the queue, or one place up or down among the rows shown.

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
)

type move int

const (
	moveTop move = iota
	moveUp
	moveDown
	moveBottom
)

// movedUp is the rows' order after the selected ones moved one place up:
// each trades places with the unselected row above it, so a selected block
// moves as one.
func movedUp(ids []string, selected func(string) bool) []string {
	out := append([]string(nil), ids...)
	for i := 1; i < len(out); i++ {
		if selected(out[i]) && !selected(out[i-1]) {
			out[i], out[i-1] = out[i-1], out[i]
		}
	}
	return out
}

// movedDown is movedUp the other way.
func movedDown(ids []string, selected func(string) bool) []string {
	out := append([]string(nil), ids...)
	for i := len(out) - 2; i >= 0; i-- {
		if selected(out[i]) && !selected(out[i+1]) {
			out[i], out[i+1] = out[i+1], out[i]
		}
	}
	return out
}

// moveSelected moves the selected rows. The table goes back to the queue's
// order first: sorted by a column, a move wouldn't show.
func (q *queueTab) moveSelected(m move) {
	var ids []string
	for _, r := range q.visible {
		if q.sel.has(r.Job.ID) {
			ids = append(ids, r.Job.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	if q.order != (sortOrder{}) {
		q.order = sortOrder{}
		q.sortArrows(q.order)
		q.render() // q.visible in the queue's order
	}
	switch m {
	case moveTop:
		q.eng.MoveToTop(ids)
	case moveBottom:
		q.eng.MoveToBottom(ids)
	case moveUp:
		q.eng.Arrange(movedUp(q.visibleIDs, q.sel.has))
	case moveDown:
		q.eng.Arrange(movedDown(q.visibleIDs, q.sel.has))
	}
	q.vm.Replace(q.eng.Jobs())
	q.render()
	for i, id := range q.visibleIDs {
		if q.sel.has(id) {
			if m == moveDown || m == moveBottom {
				continue // the last selected row is the one to show
			}
			q.list.ScrollTo(i)
			return
		}
	}
	for i := len(q.visibleIDs) - 1; i >= 0; i-- {
		if q.sel.has(q.visibleIDs[i]) {
			q.list.ScrollTo(i)
			return
		}
	}
}

// moveItems are the menu items that move the selected rows.
func (q *queueTab) moveItems(disabled bool) []*fyne.MenuItem {
	item := func(label string, m move, key fyne.KeyName) *fyne.MenuItem {
		it := fyne.NewMenuItem(label, func() { q.moveSelected(m) })
		it.Disabled = disabled
		it.Shortcut = &desktop.CustomShortcut{KeyName: key, Modifier: fyne.KeyModifierShortcutDefault}
		return it
	}
	return []*fyne.MenuItem{
		item(T("Move to top"), moveTop, fyne.KeyHome),
		item(T("Move up"), moveUp, fyne.KeyUp),
		item(T("Move down"), moveDown, fyne.KeyDown),
		item(T("Move to bottom"), moveBottom, fyne.KeyEnd),
	}
}

// bindMoveKeys: Ctrl+Up/Down move the selected rows one place, Ctrl+Home/End
// to the top or the bottom.
func (q *queueTab) bindMoveKeys() {
	for key, m := range map[fyne.KeyName]move{fyne.KeyHome: moveTop, fyne.KeyUp: moveUp, fyne.KeyDown: moveDown, fyne.KeyEnd: moveBottom} {
		q.win.Canvas().AddShortcut(&desktop.CustomShortcut{KeyName: key, Modifier: fyne.KeyModifierShortcutDefault}, func(fyne.Shortcut) {
			if q.onTable() {
				q.moveSelected(m)
			}
		})
	}
}
