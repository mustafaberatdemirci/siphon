package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/storage"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
)

// What finished is told once the queue has nothing left to do, counting only
// ends seen happening: the finished jobs a restart loads aren't news.
func TestFinishedBatch(t *testing.T) {
	vm := newViewModel()
	vm.Replace([]queue.Job{
		{ID: "old", State: queue.StateDone, Done: 5}, // from the queue file
		{ID: "a", State: queue.StateQueued},
		{ID: "b", State: queue.StateQueued},
		{ID: "c", State: queue.StateQueued},
	})
	if _, ok := vm.TakeFinishedBatch(); ok {
		t.Fatal("a batch before anything finished")
	}
	vm.Apply(queue.Job{ID: "a", State: queue.StateRunning, Done: 10})
	vm.Apply(queue.Job{ID: "a", State: queue.StateDone, Done: 100})
	if _, ok := vm.TakeFinishedBatch(); ok {
		t.Fatal("told while b and c are still queued")
	}
	vm.Apply(queue.Job{ID: "b", State: queue.StateFailed})
	vm.Apply(queue.Job{ID: "c", State: queue.StateWaiting}) // quota: may take hours
	b, ok := vm.TakeFinishedBatch()
	if !ok || b != (finishedBatch{done: 1, failed: 1, bytes: 100}) {
		t.Fatalf("got %+v %v", b, ok)
	}
	if _, ok := vm.TakeFinishedBatch(); ok {
		t.Error("the same batch was told twice")
	}
}

func TestBatchMessage(t *testing.T) {
	for b, want := range map[finishedBatch]string{
		{done: 12, bytes: 3 << 30}: "Downloaded: 12 (",
		{done: 2, failed: 1}:       "Failed: 1.",
		{failed: 3}:                "Failed: 3.",
	} {
		if got := batchMessage(b); !strings.Contains(got, want) {
			t.Errorf("%+v: %q, want it to contain %q", b, got, want)
		}
	}
	if got := batchMessage(finishedBatch{failed: 3}); strings.Contains(got, "Downloaded") {
		t.Errorf("nothing came but %q", got)
	}
}

func TestLinksInFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	recognized := func(l string) bool { return strings.Contains(l, "bunkr.") }

	list := write("links.txt", "https://bunkr.cr/a/one\n# a comment\nhttps://example.com/file.zip\n")
	if got, err := linksInFile(list, recognized); err != nil || len(got) != 2 {
		t.Errorf("list: %v %v", got, err)
	}
	// What dragging a link out of a browser onto the desktop makes.
	shortcut := write("Album.url", "[InternetShortcut]\r\nURL=https://bunkr.cr/a/two\r\n")
	if got, err := linksInFile(shortcut, recognized); err != nil || len(got) != 1 || got[0] != "https://bunkr.cr/a/two" {
		t.Errorf("shortcut: %v %v", got, err)
	}
	// A page links to everything; only the supported sites' links count.
	page := write("page.html", `<a href="https://bunkr.cr/a/three">x</a><a href="https://ads.example/x">y</a>`)
	if got, err := linksInFile(page, recognized); err != nil || len(got) != 1 || got[0] != "https://bunkr.cr/a/three" {
		t.Errorf("page: %v %v", got, err)
	}
	if _, err := linksInFile(write("movie.mp4", "https://bunkr.cr/a/four"), recognized); err == nil {
		t.Error("a video was read for links")
	}
}

// A dropped list opens the Add dialog with its links; a file without any
// says so on the status line.
func TestDropOpensAddDialog(t *testing.T) {
	q, _, _ := newTestQueue(t, "a.bin")
	dir := t.TempDir()
	list := filepath.Join(dir, "links.txt")
	if err := os.WriteFile(list, []byte("https://list.test/other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	none := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(none, []byte("nothing here"), 0o600); err != nil {
		t.Fatal(err)
	}
	q.onDropped(fyne.Position{}, []fyne.URI{storage.NewFileURI(list), storage.NewFileURI(none)})
	if q.addEntry == nil || !strings.Contains(q.addEntry.Text, "https://list.test/other") {
		t.Fatalf("the Add dialog didn't open with the dropped link")
	}
	if got := q.vm.StatusLine(); !strings.Contains(got, "notes.txt") {
		t.Errorf("the file without links wasn't told: %q", got)
	}
}
