package main

import (
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
)

func TestMovedUpAndDown(t *testing.T) {
	sel := func(set string) func(string) bool { return func(id string) bool { return strings.Contains(set, id) } }
	ids := strings.Split("abcde", "")
	for _, tc := range []struct {
		name, selected, up, down string
	}{
		{"one", "c", "acbde", "abdce"},
		{"a block moves as one", "cd", "acdbe", "abecd"},
		{"already at the edge", "a", "abcde", "bacde"},
		{"apart", "be", "baced", "acbde"},
	} {
		if got := strings.Join(movedUp(ids, sel(tc.selected)), ""); got != tc.up {
			t.Errorf("%s: up %s, want %s", tc.name, got, tc.up)
		}
		if got := strings.Join(movedDown(ids, sel(tc.selected)), ""); got != tc.down {
			t.Errorf("%s: down %s, want %s", tc.name, got, tc.down)
		}
	}
}

func shownNames(q *queueTab) string {
	var out []string
	for _, r := range q.visible {
		out = append(out, strings.TrimSuffix(r.Job.Filename, ".bin"))
	}
	return strings.Join(out, "")
}

func queueNames(eng *queue.Engine) string {
	var out []string
	for _, j := range eng.Jobs() {
		out = append(out, strings.TrimSuffix(j.Filename, ".bin"))
	}
	return strings.Join(out, "")
}

// The selected rows move in the queue, the table follows, the selection
// stays; sorted by a column, the table goes back to the queue's order.
func TestMoveSelectedRows(t *testing.T) {
	q, eng, _ := newTestQueue(t, "a.bin", "b.bin", "c.bin", "d.bin")
	q.rowPressed(q.idOf(t, "c.bin"), desktop.MouseButtonPrimary, 0)

	q.moveSelected(moveTop)
	if got := queueNames(eng); got != "cabd" {
		t.Errorf("to top: queue %s, want cabd", got)
	}
	if got := shownNames(q); got != "cabd" {
		t.Errorf("to top: table %s, want cabd", got)
	}
	q.moveSelected(moveDown)
	if got := queueNames(eng); got != "acbd" {
		t.Errorf("down: %s, want acbd", got)
	}
	q.sortBy(sortName)
	q.sortBy(sortName) // name, descending: d c b a
	q.moveSelected(moveBottom)
	if q.order != (sortOrder{}) {
		t.Error("the table stayed sorted by a column after a move")
	}
	if got := shownNames(q); got != "abdc" {
		t.Errorf("to bottom: table %s, want abdc", got)
	}
	if !q.sel.has(q.idOf(t, "c.bin")) {
		t.Error("the moved row lost its selection")
	}
}

// Ctrl+Up moves the selected rows, from the keyboard.
func TestMoveKeys(t *testing.T) {
	q, eng, w := newTestQueue(t, "a.bin", "b.bin", "c.bin")
	q.rowPressed(q.idOf(t, "c.bin"), desktop.MouseButtonPrimary, 0)
	typeShortcut(t, w, &desktop.CustomShortcut{KeyName: fyne.KeyUp, Modifier: fyne.KeyModifierShortcutDefault})
	if got := queueNames(eng); got != "acb" {
		t.Errorf("after Ctrl+Up: %s, want acb", got)
	}
}

// The search shows the rows with every word in their name or folder.
func TestSearchFiltersRows(t *testing.T) {
	q, _, _ := newTestQueue(t, "Holiday Photos 01.jpg", "holiday video.mp4", "notes.txt")
	q.search.SetText("HOLIDAY")
	if len(q.visible) != 2 {
		t.Errorf("%d rows for 'holiday', want 2", len(q.visible))
	}
	q.search.SetText("holiday mp4")
	if len(q.visible) != 1 || q.visible[0].Job.Filename != "holiday video.mp4" {
		t.Errorf("rows for 'holiday mp4': %v", shownNames(q))
	}
	q.search.SetText("")
	if len(q.visible) != 3 {
		t.Errorf("%d rows with the search cleared, want 3", len(q.visible))
	}
}
