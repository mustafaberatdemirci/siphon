package main

import (
	"strings"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

func vmWith(jobs ...queue.Job) *viewModel {
	vm := newViewModel()
	vm.mu.Lock()
	for _, j := range jobs {
		vm.apply(j, time.Now())
	}
	vm.mu.Unlock()
	return vm
}

func ids(rows []row) string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Job.ID)
	}
	return strings.Join(out, ",")
}

// Every state is under All, and under exactly one of the other filters
// except Unfinished, which gathers what isn't over.
func TestFiltersCoverEveryState(t *testing.T) {
	states := []queue.State{queue.StateQueued, queue.StateRunning, queue.StatePaused, queue.StateDone,
		queue.StateFailed, queue.StateSkipped, queue.StateStopped, queue.StateWaiting}
	for _, s := range states {
		if !filterAll.matches(s) {
			t.Errorf("%s isn't under All", s)
		}
		n := 0
		for _, f := range filters {
			if f != filterAll && f != filterUnfinished && f.matches(s) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s is under %d specific filters, want 1", s, n)
		}
		if filterUnfinished.matches(s) == s.Finished() {
			t.Errorf("%s: Unfinished = %v", s, filterUnfinished.matches(s))
		}
	}
}

func TestViewFiltersAndCounts(t *testing.T) {
	vm := vmWith(
		job("a", queue.StateRunning, 1, 10),
		job("b", queue.StateDone, 10, 10),
		job("c", queue.StateFailed, 0, 10),
		job("d", queue.StateQueued, 0, 10),
		job("e", queue.StateSkipped, 10, 10),
	)
	if got := ids(vm.View(filterFinished, sortOrder{})); got != "b,e" {
		t.Errorf("Finished shows %s", got)
	}
	if got := ids(vm.View(filterUnfinished, sortOrder{})); got != "a,c,d" {
		t.Errorf("Unfinished shows %s", got)
	}
	c := vm.Counts()
	if c[filterAll] != 5 || c[filterFinished] != 2 || c[filterFailed] != 1 || c[filterUnfinished] != 3 || c[filterPaused] != 0 {
		t.Errorf("counts = %v", c)
	}
}

func TestSortOrderCycles(t *testing.T) {
	o := sortOrder{}.next(sortSize)
	if o != (sortOrder{key: sortSize}) {
		t.Fatalf("first click: %+v", o)
	}
	if o = o.next(sortSize); o != (sortOrder{key: sortSize, desc: true}) {
		t.Fatalf("second click: %+v", o)
	}
	if o = o.next(sortSize); o != (sortOrder{}) {
		t.Fatalf("third click should go back to queue order: %+v", o)
	}
	if o = (sortOrder{key: sortSize, desc: true}).next(sortName); o != (sortOrder{key: sortName}) {
		t.Fatalf("another column starts ascending: %+v", o)
	}
}

func TestViewSorts(t *testing.T) {
	a := job("a", queue.StateRunning, 50, 100) // 50%
	a.Filename = "Day 10.mp4"
	b := job("b", queue.StateDone, 300, 300)
	b.Filename = "day 2.mp4"
	c := job("c", queue.StateQueued, 0, 200)
	c.Filename = "Day 1.mp4"
	d := job("d", queue.StateRunning, 90, 100) // 90%
	d.Filename = "Extras.zip"
	vm := vmWith(a, b, c, d)
	vm.mu.Lock()
	vm.rate["a"], vm.rate["d"] = 10, 50
	vm.mu.Unlock()

	cases := []struct {
		o    sortOrder
		want string
	}{
		{sortOrder{}, "a,b,c,d"},
		// "Day 2" before "Day 10", case ignored.
		{sortOrder{key: sortName}, "c,b,a,d"},
		{sortOrder{key: sortName, desc: true}, "d,a,b,c"},
		{sortOrder{key: sortSize}, "a,d,c,b"},
		{sortOrder{key: sortProgress, desc: true}, "b,d,a,c"},
		{sortOrder{key: sortSpeed, desc: true}, "d,a,b,c"},
		// d: 10 bytes at 50 B/s; a: 50 bytes at 10 B/s; the rest have none and go last.
		{sortOrder{key: sortETA}, "d,a,b,c"},
		{sortOrder{key: sortStatus}, "a,d,c,b"},
	}
	for _, tc := range cases {
		if got := ids(vm.View(filterAll, tc.o)); got != tc.want {
			t.Errorf("%+v: %s, want %s", tc.o, got, tc.want)
		}
	}
}

func TestNaturalLess(t *testing.T) {
	in := []string{"img10.jpg", "img2.jpg", "IMG1.jpg", "img02b.jpg", "a.jpg", "img2.jpg"}
	want := []string{"a.jpg", "IMG1.jpg", "img2.jpg", "img2.jpg", "img02b.jpg", "img10.jpg"}
	rows := make([]row, len(in))
	for i, n := range in {
		rows[i] = row{Job: queue.Job{Filename: n}}
	}
	sortRows(rows, sortOrder{key: sortName})
	for i, r := range rows {
		if r.Job.Filename != want[i] {
			t.Fatalf("got %v at %d, want %v", r.Job.Filename, i, want)
		}
	}
}

func TestProgressText(t *testing.T) {
	cases := []struct {
		j    queue.Job
		want string
	}{
		{job("a", queue.StateRunning, 50, 200), "25%"},
		{job("a", queue.StateRunning, 999, 1000), "99%"}, // never 100% before it is done
		{job("a", queue.StateRunning, 45, 1000), "4.5%"},
		{job("a", queue.StateRunning, 45, -1), ""},
		{job("a", queue.StateRunning, 0, 1000), "0.0%"},
		{job("a", queue.StateQueued, 0, 1000), ""},
		{job("a", queue.StatePaused, 0, 1000), ""},
		{job("a", queue.StateDone, 0, 0), "100%"},
		{job("a", queue.StateSkipped, 0, -1), "100%"},
	}
	for _, c := range cases {
		if got := progressText(c.j); got != c.want {
			t.Errorf("%d/%d %s: %q, want %q", c.j.Done, c.j.Size, c.j.State, got, c.want)
		}
	}
}

func TestActionsForSelection(t *testing.T) {
	a := actionsFor([]queue.Job{
		job("a", queue.StateRunning, 10, 100),
		job("b", queue.StatePaused, 20, 100),
		job("c", queue.StateFailed, 0, 100),
		job("d", queue.StateDone, 100, 100),
	})
	if a.count != 4 || a.pause != 1 || a.resume != 2 || a.unfinished != 3 || a.partial != 30 {
		t.Errorf("actions = %+v", a)
	}
	if got := counted("Remove", 3); got != "Remove (3)" {
		t.Errorf("counted = %q", got)
	}
	if got := counted("Remove", 1); got != "Remove" {
		t.Errorf("counted = %q", got)
	}
}

// "Waiting for quota" is mega's alone: it only shows while something waits,
// or while it is the filter on screen (it mustn't vanish under the user).
func TestSidebarEntries(t *testing.T) {
	has := func(es []sidebarEntry, f filter) bool {
		for _, e := range es {
			if e.f == f {
				return true
			}
		}
		return false
	}
	if has(sidebarEntries(map[filter]int{}, filterAll), filterWaiting) {
		t.Error("Waiting shown with nothing waiting")
	}
	if !has(sidebarEntries(map[filter]int{filterWaiting: 2}, filterAll), filterWaiting) {
		t.Error("Waiting hidden while jobs wait")
	}
	if !has(sidebarEntries(map[filter]int{}, filterWaiting), filterWaiting) {
		t.Error("the selected filter vanished")
	}
}

func TestSourceLinks(t *testing.T) {
	jobs := []queue.Job{
		{SourcePage: "https://mega.nz/folder/x#k"},
		{SourcePage: "https://mega.nz/folder/x#k"},
		{SourcePage: site.ViaYtDlp + "https://video.example/watch?v=1"},
		{SourcePage: site.ViaGalleryDL + "https://gallery.example/a/1"},
	}
	got := strings.Join(sourceLinks(jobs), " ")
	want := "https://mega.nz/folder/x#k https://video.example/watch?v=1 https://gallery.example/a/1"
	if got != want {
		t.Errorf("links = %q, want %q", got, want)
	}
}

// The fixed columns keep their width; Name and Status share the rest, and
// never shrink below what is readable.
func TestColumnWidths(t *testing.T) {
	sum := func(w []float32) float32 {
		var s float32
		for _, v := range w {
			s += v
		}
		return s + colGap*float32(len(w)-1)
	}
	w := columnWidths(1000)
	if d := sum(w) - 1000; d > 0.01 || d < -0.01 {
		t.Errorf("widths %v add up to %v, want 1000", w, sum(w))
	}
	for i, c := range columns {
		if c.width > 0 && w[i] != c.width {
			t.Errorf("%s: %v, want its fixed %v", c.title, w[i], c.width)
		}
	}
	if w[0] <= w[len(w)-1] {
		t.Errorf("Name (%v) should get more than Status (%v)", w[0], w[len(w)-1])
	}
	narrow := columnWidths(100)
	if narrow[0]+narrow[len(narrow)-1] < minShareArea-1 {
		t.Errorf("Name and Status squeezed to %v", narrow[0]+narrow[len(narrow)-1])
	}
}
