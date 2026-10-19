package main

// The search box above the table: only the rows whose name or folder has
// every word typed are shown. Ctrl+F goes to it.

import (
	"strings"

	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// searchWidth is the search box's width in the toolbar.
const searchWidth = 240

// matchesSearch reports whether a row's name or folder has every word of
// the search, case aside.
func matchesSearch(r row, words []string) bool {
	if len(words) == 0 {
		return true
	}
	hay := strings.ToLower(r.Job.Filename + "\n" + r.Job.Dir)
	for _, w := range words {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

// searchWords are the search's words, lowercased.
func searchWords(s string) []string { return strings.Fields(strings.ToLower(s)) }

// filterRows keeps the rows the search matches.
func filterRows(rows []row, words []string) []row {
	if len(words) == 0 {
		return rows
	}
	out := rows[:0]
	for _, r := range rows {
		if matchesSearch(r, words) {
			out = append(out, r)
		}
	}
	return out
}

// newSearch builds the search box; typing redraws the table.
func (q *queueTab) newSearch() fyne.CanvasObject {
	q.search = widget.NewEntry()
	q.search.SetPlaceHolder(T("Search"))
	clear := widget.NewButtonWithIcon("", theme.ContentClearIcon(), func() { q.search.SetText("") })
	clear.Importance = widget.LowImportance
	clear.Hide() // shown while there is something to clear
	q.search.ActionItem = clear
	q.search.OnChanged = func(text string) {
		if text == "" {
			clear.Hide()
		} else {
			clear.Show()
		}
		q.render()
		q.list.ScrollToTop()
	}
	width := canvas.NewRectangle(color.Transparent)
	width.SetMinSize(fyne.NewSize(searchWidth, 0))
	q.win.Canvas().AddShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyF, Modifier: fyne.KeyModifierShortcutDefault}, func(fyne.Shortcut) {
		q.win.Canvas().Focus(q.search)
	})
	return container.NewCenter(container.NewStack(width, q.search))
}
