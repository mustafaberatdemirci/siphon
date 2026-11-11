package main

import (
	"maps"
	"slices"
)

// Redrawing the table and the category tree without asking Fyne to refresh
// them whole each time.
//
// MEASURED 2026-11-11 (Fyne 2.8.1, a heap profile of a window left open):
// List.Refresh and Tree.Refresh each make a new item from the template
// function, only to measure it. Inside a theme override (the compact theme
// around the table and the tree, and the tree items' own count theme) Fyne
// records the theme of every object of that throwaway item in a map nothing
// removes canvas objects from, and clears its font and icon caches while
// doing so. The window redrew both up to seven times a second for as long
// as a download ran: about 110 MB of heap an hour that could never be
// freed (2.9 GB after ten hours), and the fonts measured again every time.
//
// So the whole list is refreshed only when its rows changed (another id at
// some place: a job added, removed, moved, another category or sort order);
// what a row shows is written into the row on screen. The tree is refreshed
// only when a count or the selected node changed.

// bindRow notes that the list shows row at place i. Fyne reuses rows as the
// list scrolls, so a row moves from one place to another.
func (q *queueTab) bindRow(r *jobRow, i int) {
	if q.rows == nil {
		q.rows = map[int]*jobRow{}
	}
	if r.index != i {
		if q.rows[r.index] == r {
			delete(q.rows, r.index)
		}
		r.index = i
	}
	q.rows[i] = r
}

// redrawList shows q.visible: through the rows on screen when they are the
// same jobs in the same places as before, through the list otherwise.
// UI thread only.
func (q *queueTab) redrawList() {
	if q.listIDs != nil && slices.Equal(q.visibleIDs, q.listIDs) {
		for i, r := range q.rows {
			if r.index != i || i >= len(q.visible) {
				delete(q.rows, i)
				continue
			}
			rw := q.visible[i]
			r.set(rw, q.sel.has(rw.Job.ID))
		}
		return
	}
	// Not nil even when empty: an empty list drawn once is drawn.
	q.listIDs = append(make([]string, 0, len(q.visibleIDs)), q.visibleIDs...)
	q.list.Refresh()
}

// redrawTree refreshes the category tree if what it shows changed.
// UI thread only.
func (q *queueTab) redrawTree() {
	if q.treeDrawn && q.cat == q.treeCat && maps.Equal(q.counts, q.treeCounts) {
		return
	}
	q.treeDrawn, q.treeCat, q.treeCounts = true, q.cat, maps.Clone(q.counts)
	q.tree.Refresh()
}
