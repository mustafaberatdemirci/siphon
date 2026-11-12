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
