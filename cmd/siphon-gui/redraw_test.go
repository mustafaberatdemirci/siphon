package main

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
)

// A download's progress redraws the window several times a second. Each
// List.Refresh and Tree.Refresh makes a row (or two tree items) only to
// measure it, and inside a theme override Fyne keeps them for good: the
// window grew by about 110 MB an hour. Progress must reach the rows on
// screen without the list or the tree being refreshed whole.
func TestProgressRedrawsWithoutNewRowsOrTreeItems(t *testing.T) {
	q, eng, _ := newTestQueue(t, "a.bin", "b.bin", "c.bin")
	jobs := eng.Jobs()
	if len(q.rows) != 3 {
		t.Fatalf("%d rows on screen, want 3", len(q.rows))
	}
	rows, items := q.rowsMade, treeItemsMade

	j := jobs[1]
	j.State, j.Size = queue.StateRunning, 1000
	for done := int64(100); done <= 900; done += 100 {
		j.Done = done
		q.vm.Apply(j)
		q.render()
	}
	if q.rowsMade != rows || treeItemsMade != items {
		// The first Apply moved a job from Queued to Downloading: the tree's
		// counts changed once. After that nothing may be made.
		t.Logf("rows made: %d -> %d, tree items: %d -> %d", rows, q.rowsMade, items, treeItemsMade)
	}
	rows, items = q.rowsMade, treeItemsMade
	for done := int64(910); done <= 990; done += 10 {
		j.Done = done
		q.vm.Apply(j)
		q.render()
	}
	if q.rowsMade != rows {
		t.Errorf("%d rows were made by progress alone", q.rowsMade-rows)
	}
	if treeItemsMade != items {
		t.Errorf("%d tree items were made by progress alone", treeItemsMade-items)
	}

	// The row on screen shows the progress all the same.
	r := q.rows[1]
	if r == nil || r.id != j.ID {
		t.Fatalf("the row at place 1 is not the job's: %+v", r)
	}
	if got := r.status.label.Text; got != "99%" {
		t.Errorf("status shows %q, want 99%%", got)
	}
}

// The selection is drawn by the rows on screen too, and a change of the rows
// themselves (a job removed) still goes through the list.
func TestSelectionAndRemovalRedraw(t *testing.T) {
	q, eng, _ := newTestQueue(t, "a.bin", "b.bin", "c.bin")
	b := q.idOf(t, "b.bin")
	rows := q.rowsMade
	q.rowPressed(b, desktop.MouseButtonPrimary, 0)
	if q.rowsMade != rows {
		t.Errorf("selecting a row made %d rows", q.rowsMade-rows)
	}
	if got := q.rows[1].bg.FillColor; got != theme.Color(theme.ColorNameSelection) {
		t.Errorf("the selected row's background is %v", got)
	}
	if got := q.rows[0].bg.FillColor; got == theme.Color(theme.ColorNameSelection) {
		t.Error("a row that isn't selected is drawn selected")
	}

	eng.Remove(b, false)
	q.vm.Replace(eng.Jobs())
	q.render()
	if len(q.visible) != 2 {
		t.Fatalf("%d rows shown after removing one", len(q.visible))
	}
	if q.rows[1] == nil || q.rows[1].id != q.idOf(t, "c.bin") {
		t.Error("the row after the removed one didn't move up")
	}
	if q.rows[1].name.Text != "c.bin" {
		t.Errorf("place 1 shows %q", q.rows[1].name.Text)
	}
}

// Fyne keys its font cache by the theme override a text is drawn in, and
// keeps every key: an override made per tree item or per row loaded the
// fonts again for each one made. Only the table and tree as a whole are
// in an override (one, made once).
func TestItemsMakeNoThemeOverrides(t *testing.T) {
	q, _, _ := newTestQueue(t, "a.bin")
	var walk func(o fyne.CanvasObject) int
	walk = func(o fyne.CanvasObject) int {
		n := 0
		switch c := o.(type) {
		case *container.ThemeOverride:
			n++
			n += walk(c.Content)
		case *fyne.Container:
			for _, x := range c.Objects {
				n += walk(x)
			}
		}
		return n
	}
	if n := walk(newTreeItem(false)) + walk(newTreeItem(true)); n != 0 {
		t.Errorf("a tree item holds %d theme overrides", n)
	}
	if n := walk(newJobRow(q).root); n != 0 {
		t.Errorf("a table row holds %d theme overrides", n)
	}
}
