package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
)

// filter is a sidebar entry: which jobs the table shows.
type filter int

const (
	filterAll filter = iota
	filterUnfinished
	filterActive
	filterQueued
	filterPaused
	filterWaiting
	filterFinished
	filterFailed
)

// filters is the sidebar, top to bottom.
var filters = []filter{filterAll, filterUnfinished, filterActive, filterQueued, filterPaused, filterWaiting, filterFinished, filterFailed}

func (f filter) label() string {
	switch f {
	case filterUnfinished:
		return "Unfinished"
	case filterActive:
		return "Downloading"
	case filterQueued:
		return "Queued"
	case filterPaused:
		return "Paused"
	case filterWaiting:
		return "Waiting for quota"
	case filterFinished:
		return "Finished"
	case filterFailed:
		return "Failed"
	}
	return "All"
}

func (f filter) matches(s queue.State) bool {
	switch f {
	case filterUnfinished:
		return !s.Finished()
	case filterActive:
		return s == queue.StateRunning
	case filterQueued:
		return s == queue.StateQueued
	case filterPaused:
		return s == queue.StatePaused || s == queue.StateStopped
	case filterWaiting:
		return s == queue.StateWaiting
	case filterFinished:
		return s == queue.StateDone || s == queue.StateSkipped
	case filterFailed:
		return s == queue.StateFailed
	}
	return true
}

// sortKey is the column the table is sorted by; sortNone is queue order.
type sortKey int

const (
	sortNone sortKey = iota
	sortName
	sortSize
	sortProgress
	sortSpeed
	sortETA
	sortStatus
)

type sortOrder struct {
	key  sortKey
	desc bool
}

// next is a header click: a new column sorts ascending, the same column
// again descending, a third time back to queue order.
func (o sortOrder) next(k sortKey) sortOrder {
	switch {
	case o.key != k:
		return sortOrder{key: k}
	case !o.desc:
		return sortOrder{key: k, desc: true}
	}
	return sortOrder{}
}

// View returns the rows a filter shows, in the order asked for. Equal rows,
// and every row under sortNone, keep queue order.
func (vm *viewModel) View(f filter, o sortOrder) []row {
	vm.mu.Lock()
	out := make([]row, 0, len(vm.order))
	for _, id := range vm.order {
		j := vm.jobs[id]
		if f.matches(j.State) {
			out = append(out, row{Job: j, Rate: vm.rate[id]})
		}
	}
	vm.mu.Unlock()
	sortRows(out, o)
	return out
}

// Counts is how many jobs each filter shows.
func (vm *viewModel) Counts() map[filter]int {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	c := make(map[filter]int, len(filters))
	for _, id := range vm.order {
		s := vm.jobs[id].State
		for _, f := range filters {
			if f.matches(s) {
				c[f]++
			}
		}
	}
	return c
}

func sortRows(rows []row, o sortOrder) {
	if o.key == sortNone {
		return
	}
	less := func(a, b row) bool {
		switch o.key {
		case sortName:
			return naturalLess(a.Job.Filename, b.Job.Filename)
		case sortSize:
			return a.Job.Size < b.Job.Size
		case sortProgress:
			return rowProgress(a.Job) < rowProgress(b.Job)
		case sortSpeed:
			return a.Rate < b.Rate
		case sortETA:
			return etaSeconds(a) < etaSeconds(b)
		case sortStatus:
			return stateRank(a.Job.State) < stateRank(b.Job.State)
		}
		return false
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if o.desc {
			return less(rows[j], rows[i])
		}
		return less(rows[i], rows[j])
	})
}

// etaSeconds is a row's time left for sorting: rows without one go last.
func etaSeconds(r row) float64 {
	if r.Job.State != queue.StateRunning || r.Job.Size <= 0 || r.Rate <= 0 {
		return math.Inf(1)
	}
	return float64(r.Job.Size-r.Job.Done) / r.Rate
}

// stateRank orders states by how much they need attention: what runs,
// what waits, what is left to do, then what is over.
func stateRank(s queue.State) int {
	switch s {
	case queue.StateRunning:
		return 0
	case queue.StateWaiting:
		return 1
	case queue.StateQueued:
		return 2
	case queue.StatePaused:
		return 3
	case queue.StateStopped:
		return 4
	case queue.StateFailed:
		return 5
	case queue.StateDone:
		return 6
	case queue.StateSkipped:
		return 7
	}
	return 8
}

// naturalLess compares file names the way people number them: "Day 2"
// before "Day 10", case ignored.
func naturalLess(a, b string) bool {
	ra, rb := []rune(strings.ToLower(a)), []rune(strings.ToLower(b))
	i, j := 0, 0
	for i < len(ra) && j < len(rb) {
		if unicode.IsDigit(ra[i]) && unicode.IsDigit(rb[j]) {
			si, sj := i, j
			for i < len(ra) && unicode.IsDigit(ra[i]) {
				i++
			}
			for j < len(rb) && unicode.IsDigit(rb[j]) {
				j++
			}
			na := strings.TrimLeft(string(ra[si:i]), "0")
			nb := strings.TrimLeft(string(rb[sj:j]), "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		if ra[i] != rb[j] {
			return ra[i] < rb[j]
		}
		i++
		j++
	}
	return len(ra)-i < len(rb)-j
}

// --- Cell texts (pure) ---

// sizeText is the Size column: the file's size, or what has come so far
// while the size is unknown.
func sizeText(j queue.Job) string {
	switch {
	case j.Size > 0:
		return humanBytes(j.Size)
	case j.Done > 0:
		return humanBytes(j.Done)
	}
	return ""
}

// progressText is the text on the progress bar; empty while the size is
// unknown (the bar doesn't make things up) or nothing has come yet. Rounded
// down, so an unfinished
// file never reads 100%, with a decimal below 10% so a big file visibly moves.
func progressText(j queue.Job) string {
	switch {
	case j.State == queue.StateDone || j.State == queue.StateSkipped:
		return "100%"
	case j.Size <= 0:
		return ""
	case j.Done == 0 && j.State != queue.StateRunning:
		return "" // not started: "0%" would only be noise
	}
	p := rowProgress(j) * 100
	if p < 10 {
		return fmt.Sprintf("%.1f%%", math.Floor(p*10)/10)
	}
	return fmt.Sprintf("%.0f%%", math.Floor(p))
}

// speedText is the Speed column: only while downloading.
func speedText(r row) string {
	if r.Job.State != queue.StateRunning {
		return ""
	}
	return humanRate(r.Rate)
}

// etaText is the Time left column: only while downloading a known size.
func etaText(r row) string {
	if r.Job.State != queue.StateRunning || r.Job.Size <= 0 {
		return ""
	}
	return humanETA(r.Job.Size-r.Job.Done, r.Rate)
}

// statusText is the Status column: the state, and what a person would want
// to know about it (connections, the error, when a quota wait ends).
func statusText(r row, now time.Time) string {
	j := r.Job
	switch j.State {
	case queue.StateRunning:
		if c := connsLabel(j.Conns); c != "" {
			return "Downloading  ·  " + c
		}
		return "Downloading"
	case queue.StateWaiting:
		return capitalize(waitingMeta(j, now))
	case queue.StateFailed, queue.StateStopped:
		if msg := firstLine(j.Error); msg != "" {
			return capitalize(stateLabel(j.State)) + ": " + msg
		}
	}
	return capitalize(stateLabel(j.State))
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// selActions is what the toolbar and the row menu offer for the selected
// jobs.
type selActions struct {
	count      int
	resume     int   // how many can be resumed (paused, failed, stopped, waiting)
	pause      int   // how many can be paused (running, queued)
	unfinished int   // how many removing would take out unfinished
	partial    int64 // bytes already downloaded for those: removing asks about them
}

func actionsFor(jobs []queue.Job) selActions {
	a := selActions{count: len(jobs)}
	for _, j := range jobs {
		switch actionFor(j.State) {
		case actionResume:
			a.resume++
		case actionPause:
			a.pause++
		}
		if !j.State.Finished() {
			a.unfinished++
			a.partial += j.Done
		}
	}
	return a
}

// counted adds "(n)" to a menu label when it acts on more than one job.
func counted(label string, n int) string {
	if n > 1 {
		return fmt.Sprintf("%s (%d)", label, n)
	}
	return label
}
