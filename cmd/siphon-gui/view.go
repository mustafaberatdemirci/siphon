package main

import (
	"fmt"
	"math"
	"path/filepath"
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
	sortConns
	sortAdded
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
			if ra, rb := stateRank(a.Job.State), stateRank(b.Job.State); ra != rb {
				return ra < rb
			}
			return rowProgress(a.Job) > rowProgress(b.Job)
		case sortConns:
			return a.Job.Conns < b.Job.Conns
		case sortAdded:
			return a.Job.AddedAt.Before(b.Job.AddedAt)
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

// statusText is the Status column, the way a download manager words it:
// how far a download is, or what state it is in, and for a failed job why.
func statusText(r row, now time.Time) string {
	j := r.Job
	switch j.State {
	case queue.StateRunning:
		if p := progressText(j); p != "" {
			return p
		}
		return "Downloading"
	case queue.StatePaused:
		if p := progressText(j); p != "" {
			return "Paused  ·  " + p
		}
		return "Paused"
	case queue.StateWaiting:
		return capitalize(waitingMeta(j, now))
	case queue.StateFailed, queue.StateStopped:
		if msg := firstLine(j.Error); msg != "" {
			return capitalize(stateLabel(j.State)) + ": " + msg
		}
	case queue.StateDone:
		return "Complete"
	}
	return capitalize(stateLabel(j.State))
}

// connsText is the Connections column: how many a running download really
// has.
func connsText(j queue.Job) string {
	if j.State != queue.StateRunning || j.Conns <= 0 {
		return ""
	}
	return itoa(j.Conns)
}

// addedText is the Added column: month, day and time this year, the year
// otherwise.
func addedText(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	t = t.Local()
	if t.Year() == now.Local().Year() {
		return t.Format("Jan 2 15:04")
	}
	return t.Format("Jan 2 2006")
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

// --- File kinds and the category tree ---

// fileKind groups files by what they are, the way a download manager's
// categories do.
type fileKind int

const (
	kindAny fileKind = iota
	kindVideo
	kindImage
	kindAudio
	kindArchive
	kindDocument
	kindProgram
	kindOther
)

// kinds is the order they appear in under a category.
var kinds = []fileKind{kindVideo, kindImage, kindAudio, kindArchive, kindDocument, kindProgram, kindOther}

func (k fileKind) label() string {
	switch k {
	case kindVideo:
		return "Video"
	case kindImage:
		return "Images"
	case kindAudio:
		return "Music"
	case kindArchive:
		return "Compressed"
	case kindDocument:
		return "Documents"
	case kindProgram:
		return "Programs"
	case kindOther:
		return "Other"
	}
	return "All"
}

var kindByExt = func() map[string]fileKind {
	m := map[string]fileKind{}
	add := func(k fileKind, exts ...string) {
		for _, e := range exts {
			m[e] = k
		}
	}
	add(kindVideo, "mp4", "mkv", "webm", "avi", "mov", "m4v", "wmv", "flv", "ts", "mpg", "mpeg", "3gp", "m2ts")
	add(kindImage, "jpg", "jpeg", "png", "gif", "webp", "bmp", "tif", "tiff", "heic", "avif", "svg", "psd", "raw", "cr2", "nef")
	add(kindAudio, "mp3", "flac", "wav", "m4a", "aac", "ogg", "opus", "wma", "alac", "aiff", "ape")
	add(kindArchive, "zip", "rar", "7z", "tar", "gz", "tgz", "bz2", "xz", "zst", "iso", "img", "dmg", "vpk", "cab")
	add(kindDocument, "pdf", "epub", "mobi", "azw3", "cbz", "cbr", "txt", "md", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "odt", "rtf", "csv", "djvu")
	add(kindProgram, "exe", "msi", "apk", "appimage", "deb", "rpm", "pkg", "bat", "cmd", "sh", "jar")
	return m
}()

// kindOf tells a file's kind from its extension. Split archives
// ("x.part01.rar", "x.7z.001") count as compressed.
func kindOf(name string) fileKind {
	lower := strings.ToLower(name)
	ext := strings.TrimPrefix(filepath.Ext(lower), ".")
	if k, ok := kindByExt[ext]; ok {
		return k
	}
	if len(ext) == 3 && strings.Trim(ext, "0123456789") == "" {
		if inner := strings.TrimPrefix(filepath.Ext(strings.TrimSuffix(lower, "."+ext)), "."); kindByExt[inner] == kindArchive {
			return kindArchive
		}
	}
	return kindOther
}

// category is a node of the sidebar tree: a state group and a file kind.
type category struct {
	f filter
	k fileKind
}

func (c category) matches(j queue.Job) bool {
	return c.f.matches(j.State) && (c.k == kindAny || kindOf(j.Filename) == c.k)
}

// The tree, a download manager's: All downloads with the file kinds under
// it, Unfinished with the states, and Finished:
//
//	all            all/kind/1 …           (kinds)
//	unfinished     unfinished/state/2 …   (states)
//	finished
const (
	nodeAll        = "all"
	nodeUnfinished = "unfinished"
	nodeFinished   = "finished"
)

var topNodes = []string{nodeAll, nodeUnfinished, nodeFinished}

var topFilter = map[string]filter{nodeAll: filterAll, nodeUnfinished: filterUnfinished, nodeFinished: filterFinished}

// stateNodes are the states listed under Unfinished.
var stateNodes = []filter{filterActive, filterQueued, filterPaused, filterWaiting, filterFailed}

func stateNodeID(f filter) string { return nodeUnfinished + "/state/" + itoa(int(f)) }
func kindNodeID(top string, k fileKind) string {
	return top + "/kind/" + itoa(int(k))
}

func itoa(n int) string { return fmt.Sprint(n) }

// categoryOf reads a node id; unknown ids are All downloads.
func categoryOf(id string) category {
	top, rest, _ := strings.Cut(id, "/")
	c := category{f: topFilter[top], k: kindAny}
	if _, ok := topFilter[top]; !ok {
		return category{f: filterAll}
	}
	part, num, _ := strings.Cut(rest, "/")
	var n int
	fmt.Sscan(num, &n)
	switch part {
	case "state":
		c.f = filter(n)
	case "kind":
		c.k = fileKind(n)
	}
	return c
}

// categoryLabel is how a node reads in the tree.
func categoryLabel(id string) string {
	switch id {
	case nodeAll:
		return "All downloads"
	case nodeUnfinished:
		return "Unfinished"
	case nodeFinished:
		return "Finished"
	}
	c := categoryOf(id)
	if c.k != kindAny {
		return c.k.label()
	}
	return c.f.label()
}

// treeChildren lists a node's children. Kinds and states with nothing in
// them are left out, as is Waiting for quota (mega's alone), unless the
// node is the one selected: it mustn't vanish under the user.
func treeChildren(id string, counts map[string]int, selected string) []string {
	if id == "" {
		return topNodes
	}
	var out []string
	keep := func(child string) {
		if counts[child] > 0 || child == selected {
			out = append(out, child)
		}
	}
	switch id {
	case nodeAll:
		for _, k := range kinds {
			keep(kindNodeID(id, k))
		}
	case nodeUnfinished:
		for _, f := range stateNodes {
			keep(stateNodeID(f))
		}
	}
	return out
}

// isTreeBranch: All downloads and Unfinished open; Finished is a leaf.
func isTreeBranch(id string) bool {
	return id == "" || id == nodeAll || id == nodeUnfinished
}

// CategoryCounts is how many jobs every tree node shows.
func (vm *viewModel) CategoryCounts() map[string]int {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	c := map[string]int{}
	for _, id := range vm.order {
		j := vm.jobs[id]
		k := kindOf(j.Filename)
		for _, top := range topNodes {
			if !topFilter[top].matches(j.State) {
				continue
			}
			c[top]++
			c[kindNodeID(top, k)]++
		}
		for _, f := range stateNodes {
			if f.matches(j.State) {
				c[stateNodeID(f)]++
			}
		}
	}
	return c
}

// ViewCategory returns the rows a tree node shows, in the order asked for.
func (vm *viewModel) ViewCategory(c category, o sortOrder) []row {
	vm.mu.Lock()
	out := make([]row, 0, len(vm.order))
	for _, id := range vm.order {
		j := vm.jobs[id]
		if c.matches(j) {
			out = append(out, row{Job: j, Rate: vm.rate[id]})
		}
	}
	vm.mu.Unlock()
	sortRows(out, o)
	return out
}
