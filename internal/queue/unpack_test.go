package queue

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, c := range files {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(c))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// noticeLog collects the engine's notices.
type noticeLog struct {
	mu   sync.Mutex
	list []string
}

func (n *noticeLog) add(s string) { n.mu.Lock(); n.list = append(n.list, s); n.mu.Unlock() }

// all is a copy of the notices so far.
func (n *noticeLog) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.list...)
}

// count is how many notices have part in them.
func (n *noticeLog) count(part string) int {
	c := 0
	for _, s := range n.all() {
		if strings.Contains(s, part) {
			c++
		}
	}
	return c
}

func (n *noticeLog) waitFor(t *testing.T, part string) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		n.mu.Lock()
		for _, s := range n.list {
			if strings.Contains(s, part) {
				n.mu.Unlock()
				return s
			}
		}
		n.mu.Unlock()
	}
	t.Fatalf("no notice with %q; got %v", part, n.list)
	return ""
}

// A finished zip is unpacked next to itself, and deleted after when asked.
func TestFinishedArchiveIsUnpacked(t *testing.T) {
	for _, del := range []bool{false, true} {
		f := newFakeSite(t, "photos.zip")
		f.files["photos.zip"] = zipOf(t, map[string]string{"photos/a.jpg": "A", "photos/b.jpg": "B"})
		h := newHarness(t, f, "", 1)
		var notes noticeLog
		h.e.opt.OnNotice = notes.add
		h.e.SetUnpack(true, del)
		if _, err := h.e.Add(context.Background(), "file://photos.zip", h.out); err != nil {
			t.Fatal(err)
		}
		h.start()
		h.waitAll(StateDone, 10*time.Second)
		notes.waitFor(t, "Unpacked photos.zip into photos")
		h.stop()

		got, err := os.ReadFile(filepath.Join(h.out, "Album", "photos", "b.jpg"))
		if err != nil || string(got) != "B" {
			t.Errorf("delete=%v: the unpacked file: %q, %v", del, got, err)
		}
		_, err = os.Stat(filepath.Join(h.out, "Album", "photos.zip"))
		if del != os.IsNotExist(err) {
			t.Errorf("delete=%v: the archive is there: %v", del, err == nil)
		}
	}
}

// Off (the default), nothing is unpacked.
func TestUnpackingIsOffByDefault(t *testing.T) {
	f := newFakeSite(t, "photos.zip")
	f.files["photos.zip"] = zipOf(t, map[string]string{"a.jpg": "A"})
	h := newHarness(t, f, "", 1)
	if _, err := h.e.Add(context.Background(), "file://photos.zip", h.out); err != nil {
		t.Fatal(err)
	}
	h.start()
	h.waitAll(StateDone, 10*time.Second)
	time.Sleep(200 * time.Millisecond)
	h.stop()
	entries, _ := os.ReadDir(filepath.Join(h.out, "Album"))
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("something was unpacked: %s", e.Name())
		}
	}
}

// An archive in parts is unpacked once, when its last part in the queue is
// done, from its first part.
func TestArchiveInPartsWaitsForEveryPart(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	h := newHarness(t, f, "", 1)
	e := h.e
	e.SetUnpack(true, false)
	mk := func(name string, st State) *Job {
		j := &Job{ID: name, OutDir: "out", Dir: "Album", Filename: name, Path: filepath.Join("out", "Album", name), State: st}
		e.jobs = append(e.jobs, j)
		e.byID[j.ID] = j
		return j
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	p1 := mk("Show.part1.rar", StateDone)
	p2 := mk("Show.part2.rar", StateRunning)
	mk("Other.zip", StateQueued)
	if _, _, ok := e.unpackJob(p1); ok {
		t.Fatal("unpacking began with a part still downloading")
	}
	p2.State = StateDone
	first, parts, ok := e.unpackJob(p2)
	if !ok || filepath.Base(first) != "Show.part1.rar" || len(parts) != 2 {
		t.Fatalf("with every part done: %q, %v, %v", first, parts, ok)
	}
	if _, _, ok := e.unpackJob(p1); ok {
		t.Error("the same archive was to be unpacked twice")
	}
}

// The parts of a first run have no path yet while they download or wait in
// the queue: their names still make them parts of the set, and the set
// waits for them.
func TestArchiveInPartsWaitsForPartsWithoutAPath(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	h := newHarness(t, f, "", 1)
	e := h.e
	e.SetUnpack(true, false)
	e.mu.Lock()
	defer e.mu.Unlock()
	p1 := &Job{ID: "1", OutDir: "out", Dir: "Album", Filename: "Show.part1.rar", Path: filepath.Join("out", "Album", "Show.part1.rar"), State: StateDone}
	p2 := &Job{ID: "2", OutDir: "out", Dir: "Album", Filename: "Show.part2.rar", State: StateRunning}
	p3 := &Job{ID: "3", OutDir: "out", Dir: "Album", Filename: "Show.part3.rar", State: StateQueued}
	for _, j := range []*Job{p1, p2, p3} {
		e.jobs = append(e.jobs, j)
		e.byID[j.ID] = j
	}
	if _, _, ok := e.unpackJob(p1); ok {
		t.Fatal("unpacking began with a part downloading and one queued")
	}
	p2.State, p2.Path = StateDone, filepath.Join("out", "Album", "Show.part2.rar")
	if _, _, ok := e.unpackJob(p2); ok {
		t.Fatal("unpacking began with a part queued")
	}
	p3.State, p3.Path = StateDone, filepath.Join("out", "Album", "Show.part3.rar")
	first, parts, ok := e.unpackJob(p3)
	if !ok || filepath.Base(first) != "Show.part1.rar" || len(parts) != 3 {
		t.Fatalf("with every part done: %q, %v, %v", first, parts, ok)
	}
}

// A part the ledger already had (skipped) is there as much as one
// downloaded now: the set is unpacked.
func TestArchiveInPartsCountsSkippedParts(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	h := newHarness(t, f, "", 1)
	e := h.e
	e.SetUnpack(true, false)
	e.mu.Lock()
	defer e.mu.Unlock()
	p1 := &Job{ID: "1", OutDir: "out", Dir: "Album", Filename: "Show.7z.001", Path: filepath.Join("out", "Album", "Show.7z.001"), State: StateSkipped}
	p2 := &Job{ID: "2", OutDir: "out", Dir: "Album", Filename: "Show.7z.002", Path: filepath.Join("out", "Album", "Show.7z.002"), State: StateDone}
	for _, j := range []*Job{p1, p2} {
		e.jobs = append(e.jobs, j)
		e.byID[j.ID] = j
	}
	first, parts, ok := e.unpackJob(p2)
	if !ok || filepath.Base(first) != "Show.7z.001" || len(parts) != 2 {
		t.Fatalf("with one part skipped and one done: %q, %v, %v", first, parts, ok)
	}
}

// Through the engine: the parts download at the same time and the last one
// is held. The set is unpacked once, when that last part is done. Before,
// the first part to finish had the set unpacked while the others were still
// coming (their jobs had no path yet), and it was never tried again.
func TestArchiveInPartsUnpacksOnceTheLastPartIsDone(t *testing.T) {
	names := []string{"Show.part1.rar", "Show.part2.rar", "Show.part3.rar"}
	f := newFakeSite(t, names...)
	hold := f.holdNext("Show.part3.rar")
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	// Before the fake site's own cleanup (cleanups run last first): a held
	// answer would keep its server from closing.
	t.Cleanup(release)

	h := newHarness(t, f, "", len(names))
	var notes noticeLog
	h.e.opt.OnNotice = notes.add
	h.e.SetUnpack(true, false)
	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	h.start()
	defer h.stop()

	h.waitState(jobByName(h.e, "Show.part1.rar").ID, StateDone, 10*time.Second)
	h.waitState(jobByName(h.e, "Show.part2.rar").ID, StateDone, 10*time.Second)
	if got := jobByName(h.e, "Show.part3.rar"); got.State != StateRunning {
		t.Fatalf("the held part is %s, want running", got.State)
	}
	// Unpacking runs in a goroutine of its own: give it time to show.
	time.Sleep(300 * time.Millisecond)
	if n := notes.count("Unpacking"); n != 0 {
		t.Fatalf("the set was unpacked with a part still downloading: %q", notes.all())
	}

	release()
	h.waitAll(StateDone, 15*time.Second)
	notes.waitFor(t, "Unpacking Show.part1.rar")
	time.Sleep(300 * time.Millisecond)
	if n := notes.count("Unpacking"); n != 1 {
		t.Errorf("the set was unpacked %d times, want once: %q", n, notes.all())
	}
}
