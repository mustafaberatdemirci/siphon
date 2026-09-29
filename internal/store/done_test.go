package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestKeyDoesNotDependOnURL(t *testing.T) {
	// The key's signature has NO URL. bunkr's signed address changes on every
	// run; keying by URL would break idempotent restarts every time.
	// This test pins the signature itself: if a URL is added to Key it won't
	// compile.
	a := Key("album", "https://bunkr.ws/f/slug", "file.mp4")
	b := Key("album", "https://bunkr.ws/f/slug", "file.mp4")
	if a != b {
		t.Fatalf("the same item produced different keys: %q vs %q", a, b)
	}
	if a == "" {
		t.Fatal("empty key")
	}
}

// Plain concatenation ("a"+"bc" vs "ab"+"c") could map two different items
// to the same key. File names can contain any character, so there is no safe
// separator; that is why there is a length prefix.
func TestKeyIsUnambiguous(t *testing.T) {
	pairs := [][2][3]string{
		{{"a", "bc", "x"}, {"ab", "c", "x"}},
		{{"", "ab", "c"}, {"a", "b", "c"}},
		{{"a|b", "c", "d"}, {"a", "b|c", "d"}},
		{{"x", "", ""}, {"", "x", ""}},
	}
	for _, p := range pairs {
		k1 := Key(p[0][0], p[0][1], p[0][2])
		k2 := Key(p[1][0], p[1][1], p[1][2])
		if k1 == k2 {
			t.Errorf("collision: %v and %v give the same key (%s)", p[0], p[1], k1)
		}
	}
}

func TestOpenPlacesLedgerUnderOutRoot(t *testing.T) {
	// The ledger must live UNDER THE OUTPUT ROOT. If it were in the cwd or
	// next to the exe, a second run with a different -out would say
	// "everything is downloaded" and return 0 without downloading anything.
	out := filepath.Join(t.TempDir(), "output", "sub")
	l, err := Open(out)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	want := filepath.Join(out, FileName)
	if l.Path() != want {
		t.Fatalf("ledger path = %q, want %q", l.Path(), want)
	}
	if _, serr := os.Stat(want); serr != nil {
		t.Fatalf("ledger file was not created: %v", serr)
	}
}

func TestAddAndLookup(t *testing.T) {
	out := t.TempDir()
	l, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	e := Entry{
		SourcePage: "https://bunkr.ws/f/slug",
		Dir:        "Album 2026",
		Filename:   "one.mp4",
		Path:       filepath.Join("Album 2026", "one.mp4"),
		Size:       1234,
		SHA256:     "deadbeef",
	}
	if err := l.Add(e); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if l.Len() != 1 {
		t.Fatalf("Len = %d, want 1", l.Len())
	}

	got, ok := l.Lookup(e.Dir, e.SourcePage, e.Filename)
	if !ok {
		t.Fatal("the added item was not found")
	}
	if got.Path != e.Path || got.Size != 1234 || got.SHA256 != "deadbeef" {
		t.Errorf("corrupted entry: %+v", got)
	}
	if got.TS == "" {
		t.Error("timestamp was not filled in")
	}

	if _, ok := l.Lookup("other", e.SourcePage, e.Filename); ok {
		t.Error("a different dir mapped to the same key")
	}
}

// The real criterion: the ledger survives ACROSS RUNS.
func TestLedgerSurvivesReopen(t *testing.T) {
	out := t.TempDir()
	l, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := l.Add(Entry{
			SourcePage: fmt.Sprintf("https://s/%d", i),
			Dir:        "d",
			Filename:   fmt.Sprintf("f%d.bin", i),
			Path:       fmt.Sprintf("d/f%d.bin", i),
			Size:       int64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	l2, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if l2.Len() != 3 {
		t.Fatalf("Len on reopen = %d, want 3", l2.Len())
	}
	if _, ok := l2.Lookup("d", "https://s/1", "f1.bin"); !ok {
		t.Error("the previous run's entry could not be read")
	}
	// Appends are append-only: old lines must be preserved.
	data, _ := os.ReadFile(l2.Path())
	if n := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; n != 3 {
		t.Errorf("%d lines present, want 3", n)
	}
}

// A last line half-written during a crash is an EXPECTED state. Treating the
// ledger as corrupt and failing the run would turn a recoverable state into a
// fatal one. But it doesn't pass silently either: it is counted.
func TestCorruptLinesAreSkippedNotFatal(t *testing.T) {
	out := t.TempDir()
	path := filepath.Join(out, FileName)
	good, _ := json.Marshal(Entry{
		SourcePage: "https://s/1", Dir: "d", Filename: "good.bin", Path: "d/good.bin",
	})
	content := string(good) + "\n" +
		"{this is not valid json\n" +
		"\n" +
		`{"source_page":"https://s/2","dir":"d"}` + "\n" + // no filename
		`{"source_page":"https://s/3","dir":"d","filename":"half.bin"` // unterminated
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := Open(out)
	if err != nil {
		t.Fatalf("a corrupt line should not have failed the run: %v", err)
	}
	defer l.Close()

	if l.Len() != 1 {
		t.Fatalf("Len = %d, only the good line should have been read", l.Len())
	}
	if _, ok := l.Lookup("d", "https://s/1", "good.bin"); !ok {
		t.Error("the good line was lost")
	}
	// The empty line isn't counted; the other three lines are corrupt.
	if l.Skipped() != 3 {
		t.Errorf("Skipped = %d, want 3", l.Skipped())
	}
}

// The first entry added AFTER a half-written last line must not be lost.
// Previously the new line was glued onto the half line; together they formed
// one corrupt line and the new entry could not be read on the next open
// either.
func TestAddAfterTruncatedLastLineSurvivesReopen(t *testing.T) {
	out := t.TempDir()
	path := filepath.Join(out, FileName)
	good, _ := json.Marshal(Entry{SourcePage: "https://s/1", Filename: "a.bin", Path: "a.bin"})
	content := string(good) + "\n" + `{"source_page":"https://s/2","fil` // cut off by a crash
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Add(Entry{SourcePage: "https://s/3", Filename: "c.bin", Path: "c.bin"}); err != nil {
		t.Fatal(err)
	}
	l.Close()

	l2, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if _, ok := l2.Lookup("", "https://s/3", "c.bin"); !ok {
		t.Error("the entry added after the half line was lost on reopen")
	}
	if l2.Len() != 2 {
		t.Errorf("Len = %d, want 2", l2.Len())
	}
	if l2.Skipped() != 1 {
		t.Errorf("Skipped = %d, only the half line should be counted", l2.Skipped())
	}
}

// Long file names and paths can exceed the default 64 KB scanner limit.
func TestLongLinesAreRead(t *testing.T) {
	out := t.TempDir()
	long := strings.Repeat("u", 200000)
	e := Entry{SourcePage: "https://s/1", Dir: long, Filename: "f.bin", Path: "f.bin"}
	data, _ := json.Marshal(e)
	if err := os.WriteFile(filepath.Join(out, FileName), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.Len() != 1 {
		t.Fatalf("the long line could not be read (Len=%d, Skipped=%d)", l.Len(), l.Skipped())
	}
}

func TestAddRequiresFilename(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Add(Entry{SourcePage: "https://s", Dir: "d"}); err == nil {
		t.Fatal("an entry without a file name was accepted")
	}
}

func TestAddAfterCloseFails(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Add(Entry{Filename: "f.bin"}); err == nil {
		t.Fatal("wrote to a closed ledger")
	}
	// Close must be idempotent.
	if err := l.Close(); err != nil {
		t.Errorf("second Close returned an error: %v", err)
	}
}

// Downloads run concurrently; the ledger will be written concurrently too.
func TestConcurrentAdd(t *testing.T) {
	out := t.TempDir()
	l, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}

	const n = 40
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = l.Add(Entry{
				SourcePage: fmt.Sprintf("https://s/%d", i),
				Dir:        "d",
				Filename:   fmt.Sprintf("f%d.bin", i),
				Path:       fmt.Sprintf("d/f%d.bin", i),
			})
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("item %d: %v", i, e)
		}
	}
	if l.Len() != n {
		t.Fatalf("Len = %d, want %d", l.Len(), n)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// Lines must not be interleaved: every line is valid JSON.
	l2, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if l2.Skipped() != 0 {
		t.Errorf("%d lines corrupted; concurrent writes interleave lines", l2.Skipped())
	}
	if l2.Len() != n {
		t.Errorf("Len on reopen = %d, want %d", l2.Len(), n)
	}
}

func TestOpenFailsOnUnusableRoot(t *testing.T) {
	// Giving an existing FILE as the output root must fail; silently
	// continuing without a ledger would silently switch off idempotence.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f); err == nil {
		t.Fatal("Open succeeded on a file path")
	}
}
