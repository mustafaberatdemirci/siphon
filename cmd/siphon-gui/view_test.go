package main

import (
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"

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

// The tree's states under Unfinished, with Finished, split every state
// between them: each job is under exactly one.
func TestTreeStatesCoverEveryState(t *testing.T) {
	states := []queue.State{queue.StateQueued, queue.StateRunning, queue.StatePaused, queue.StateDone,
		queue.StateFailed, queue.StateSkipped, queue.StateStopped, queue.StateWaiting}
	for _, s := range states {
		if !filterAll.matches(s) {
			t.Errorf("%s isn't under All", s)
		}
		n := 0
		for _, f := range append(append([]filter{}, stateNodes...), filterFinished) {
			if f.matches(s) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s is under %d of the states and Finished, want 1", s, n)
		}
		if filterUnfinished.matches(s) == s.Finished() {
			t.Errorf("%s: Unfinished = %v", s, filterUnfinished.matches(s))
		}
	}
}

func named(id string, st queue.State, name string) queue.Job {
	j := job(id, st, 0, 10)
	j.Filename = name
	return j
}

func TestCategoryViewAndCounts(t *testing.T) {
	vm := vmWith(
		named("a", queue.StateRunning, "clip.mp4"),
		named("b", queue.StateDone, "photo.jpg"),
		named("c", queue.StateFailed, "setup.exe"),
		named("d", queue.StateQueued, "film.mkv"),
		named("e", queue.StateSkipped, "album.part01.rar"),
	)
	cases := map[string]string{
		nodeAll:                               "a,b,c,d,e",
		nodeFinished:                          "b,e",
		nodeUnfinished:                        "a,c,d",
		kindNodeID(nodeAll, kindVideo):        "a,d",
		kindNodeID(nodeFinished, kindArchive): "e",
		kindNodeID(nodeUnfinished, kindVideo): "a,d",
		stateNodeID(filterFailed):             "c",
		stateNodeID(filterActive):             "a",
	}
	for id, want := range cases {
		if got := ids(vm.ViewCategory(categoryOf(id), sortOrder{})); got != want {
			t.Errorf("%s (%s) shows %s, want %s", id, categoryLabel(id), got, want)
		}
	}
	c := vm.CategoryCounts()
	if c[nodeAll] != 5 || c[nodeFinished] != 2 || c[nodeUnfinished] != 3 ||
		c[kindNodeID(nodeAll, kindImage)] != 1 || c[stateNodeID(filterPaused)] != 0 {
		t.Errorf("counts = %v", c)
	}
}

func TestCategoryIDs(t *testing.T) {
	for _, id := range []string{nodeAll, nodeUnfinished, nodeFinished,
		stateNodeID(filterPaused), kindNodeID(nodeFinished, kindAudio), kindNodeID(nodeUnfinished, kindProgram)} {
		c := categoryOf(id)
		if categoryLabel(id) == "" {
			t.Errorf("%s has no label", id)
		}
		switch {
		case id == stateNodeID(filterPaused) && (c.f != filterPaused || c.k != kindAny),
			id == kindNodeID(nodeFinished, kindAudio) && (c.f != filterFinished || c.k != kindAudio):
			t.Errorf("%s reads as %+v", id, c)
		}
	}
	if c := categoryOf("nonsense/kind/3"); c.f != filterAll || c.k != kindAny {
		t.Errorf("an unknown id reads as %+v, want All downloads", c)
	}
}

func TestKindOf(t *testing.T) {
	cases := map[string]fileKind{
		"Holiday.MP4": kindVideo, "a.webm": kindVideo, "photo.jpeg": kindImage, "song.flac": kindAudio,
		"backup.zip": kindArchive, "disk.iso": kindArchive, "x.part01.rar": kindArchive, "x.7z.001": kindArchive,
		"book.epub": kindDocument, "setup.exe": kindProgram, "app.apk": kindProgram,
		"README": kindOther, "data.bin": kindOther, "notes.001": kindOther,
	}
	for name, want := range cases {
		if got := kindOf(name); got != want {
			t.Errorf("kindOf(%q) = %s, want %s", name, got.label(), want.label())
		}
	}
}

// Kinds and states with nothing in them stay out of the tree, unless the
// node is the one selected (it mustn't vanish under the user).
func TestTreeChildren(t *testing.T) {
	counts := map[string]int{
		kindNodeID(nodeAll, kindVideo):        2,
		stateNodeID(filterActive):             1,
		kindNodeID(nodeUnfinished, kindVideo): 1,
	}
	if got := treeChildren("", counts, nodeAll); strings.Join(got, ",") != "all,unfinished,finished" {
		t.Errorf("top = %v", got)
	}
	if got := treeChildren(nodeAll, counts, nodeAll); len(got) != 1 || got[0] != kindNodeID(nodeAll, kindVideo) {
		t.Errorf("under All: %v", got)
	}
	// Unfinished lists states only, and Finished nothing: a short tree.
	if got := treeChildren(nodeUnfinished, counts, nodeAll); strings.Join(got, ",") != stateNodeID(filterActive) {
		t.Errorf("under Unfinished: %v", got)
	}
	if got := treeChildren(nodeFinished, counts, nodeAll); got != nil || isTreeBranch(nodeFinished) {
		t.Errorf("Finished has children: %v", got)
	}
	waiting := stateNodeID(filterWaiting)
	if got := treeChildren(nodeUnfinished, counts, waiting); !strings.Contains(strings.Join(got, ","), waiting) {
		t.Errorf("the selected node vanished: %v", got)
	}
	if got := treeChildren(kindNodeID(nodeAll, kindVideo), counts, nodeAll); got != nil {
		t.Errorf("a leaf has children: %v", got)
	}
}

func TestColumnTextsAddedAndConns(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
	if got := addedText(time.Date(2026, 6, 17, 23, 4, 0, 0, time.Local), now); got != "Jun 17 23:04" {
		t.Errorf("this year: %q", got)
	}
	if got := addedText(time.Date(2025, 12, 1, 9, 0, 0, 0, time.Local), now); got != "Dec 1 2025" {
		t.Errorf("last year: %q", got)
	}
	if got := addedText(time.Time{}, now); got != "" {
		t.Errorf("no time: %q", got)
	}
	j := job("a", queue.StateRunning, 1, 10)
	j.Conns = 8
	if got := connsText(j); got != "8" {
		t.Errorf("running with 8: %q", got)
	}
	j.State = queue.StatePaused
	if got := connsText(j); got != "" {
		t.Errorf("paused: %q", got)
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
		// Same state: further along first.
		{sortOrder{key: sortStatus}, "d,a,c,b"},
	}
	for _, tc := range cases {
		if got := ids(vm.ViewCategory(category{f: filterAll}, tc.o)); got != tc.want {
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
		if c.width > 0 && w[i] != fixedWidth(i) {
			t.Errorf("%s: %v, want its fixed %v", c.title, w[i], fixedWidth(i))
		}
	}
	// A fixed column is never narrower than its title, in any language.
	defer func(l language) { current = l }(current)
	for _, l := range []language{langEnglish, langTurkish, langGerman} {
		current = l
		for i, c := range columns {
			if c.width == 0 {
				continue
			}
			need := fyne.MeasureText(T(c.title), 13, fyne.TextStyle{Bold: true}).Width
			if got := columnWidths(1000)[i]; got < need {
				t.Errorf("%s: %q needs %v, the column has %v", l, T(c.title), need, got)
			}
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
