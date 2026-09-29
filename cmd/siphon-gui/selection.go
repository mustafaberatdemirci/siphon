package main

// selection is which jobs are selected in the table, by job id, so it
// survives sorting, filtering and jobs changing state. anchor is where a
// Shift-click range starts: the last row clicked without Shift.
type selection struct {
	ids    map[string]bool
	anchor string
}

func newSelection() *selection { return &selection{ids: map[string]bool{}} }

// click selects only id.
func (s *selection) click(id string) {
	s.ids = map[string]bool{id: true}
	s.anchor = id
}

// toggle adds or removes id (Ctrl-click).
func (s *selection) toggle(id string) {
	if s.ids[id] {
		delete(s.ids, id)
	} else {
		s.ids[id] = true
	}
	s.anchor = id
}

// extend selects the rows from the anchor to id in the order shown
// (Shift-click). Without an anchor on screen it is a plain click.
func (s *selection) extend(order []string, id string) {
	from, to := -1, -1
	for i, v := range order {
		if v == s.anchor {
			from = i
		}
		if v == id {
			to = i
		}
	}
	if from < 0 || to < 0 {
		s.click(id)
		return
	}
	if from > to {
		from, to = to, from
	}
	s.ids = map[string]bool{}
	for _, v := range order[from : to+1] {
		s.ids[v] = true
	}
}

// all selects every row shown.
func (s *selection) all(order []string) {
	s.ids = make(map[string]bool, len(order))
	for _, v := range order {
		s.ids[v] = true
	}
	if len(order) > 0 {
		s.anchor = order[0]
	}
}

func (s *selection) clear() {
	s.ids = map[string]bool{}
	s.anchor = ""
}

func (s *selection) has(id string) bool { return s.ids[id] }

// keepOnly drops ids that aren't in order: rows filtered out, removed or
// cleared stop being selected, so an action never reaches a job the user
// can't see.
func (s *selection) keepOnly(order []string) {
	shown := make(map[string]bool, len(order))
	for _, v := range order {
		shown[v] = true
	}
	for id := range s.ids {
		if !shown[id] {
			delete(s.ids, id)
		}
	}
	if !shown[s.anchor] {
		s.anchor = ""
	}
}

// in returns the selected ids in the order shown.
func (s *selection) in(order []string) []string {
	var out []string
	for _, v := range order {
		if s.ids[v] {
			out = append(out, v)
		}
	}
	return out
}
