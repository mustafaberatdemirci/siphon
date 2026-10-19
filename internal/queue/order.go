package queue

// The queue's order is the order queued jobs start in: a free slot goes to
// the first queued job in the list. The user changes it to put a file
// before a large album added earlier. A running job isn't interrupted; the
// order counts for the next free slot.

// MoveToTop puts the jobs ids names first in the queue, in their present
// order among themselves.
func (e *Engine) MoveToTop(ids []string) { e.moveTo(ids, true) }

// MoveToBottom puts them last.
func (e *Engine) MoveToBottom(ids []string) { e.moveTo(ids, false) }

func (e *Engine) moveTo(ids []string, top bool) {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	e.mu.Lock()
	moved := make([]*Job, 0, len(ids))
	rest := make([]*Job, 0, len(e.jobs))
	for _, j := range e.jobs {
		if want[j.ID] {
			moved = append(moved, j)
		} else {
			rest = append(rest, j)
		}
	}
	if len(moved) == 0 {
		e.mu.Unlock()
		return
	}
	if top {
		e.jobs = append(moved, rest...)
	} else {
		e.jobs = append(rest, moved...)
	}
	e.mu.Unlock()
	e.scheduleSave()
	e.kick()
}

// Arrange puts the jobs ids names back into the places they hold now, in
// the order given; every other job keeps its place. A window showing some
// of the jobs (a category) moves them among themselves this way. Unknown
// ids are left out; jobs named twice count once.
func (e *Engine) Arrange(ids []string) {
	e.mu.Lock()
	order := make([]*Job, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if j, ok := e.byID[id]; ok && !seen[id] {
			seen[id] = true
			order = append(order, j)
		}
	}
	next := 0
	for i, j := range e.jobs {
		if seen[j.ID] {
			e.jobs[i] = order[next]
			next++
		}
	}
	e.mu.Unlock()
	if next > 0 {
		e.scheduleSave()
		e.kick()
	}
}
