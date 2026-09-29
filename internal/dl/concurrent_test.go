package dl

// Since step 6 Download is called concurrently. Independent of -race, these
// tests chase race conditions through observable RESULTS: hundreds of
// concurrent downloads with colliding names, the expected number of files
// and correct content.
//
// If claim()'s lock is removed these tests fail because the file count is off.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentDownloadsDistinctNames(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	const n = 64
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			it := testItem(srv.URL+"/data.bin", fmt.Sprintf("file-%03d.bin", i))
			it.Index = i
			it.SHA256 = payloadSHA()
			_, errs[i] = d.Download(context.Background(), out, it)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}
	entries := ownEntries(t, out)
	if len(entries) != n {
		t.Fatalf("%d files created, want %d", len(entries), n)
	}
}

// The real race is here: they all resolve to the SAME name, i.e. they write
// into the claim() map at the same time. Without the lock two items take the
// same name and one overwrites the other; the result is fewer than n files.
func TestConcurrentDownloadsCollidingNames(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	const n = 48
	var wg sync.WaitGroup
	errs := make([]error, n)
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			it := testItem(srv.URL+"/data.bin", "same.bin")
			it.Index = i
			it.SourcePage = fmt.Sprintf("https://example.test/u/%d", i) // different items
			it.SHA256 = payloadSHA()
			var res Result
			res, errs[i] = d.Download(context.Background(), out, it)
			paths[i] = res.Path
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}

	// Every item must have gotten a path of its own.
	seen := map[string]bool{}
	for i, p := range paths {
		if p == "" {
			t.Fatalf("item %d returned no path", i)
		}
		if seen[p] {
			t.Fatalf("two items got the same path: %s", p)
		}
		seen[p] = true
	}

	entries := ownEntries(t, out)
	if len(entries) != n {
		t.Fatalf("%d files created, want %d; claim() has a race", len(entries), n)
	}

	// The content must be right too: an overwrite would already fail the
	// hash check, but we separately verify the files really are complete.
	for _, e := range entries {
		b, rerr := os.ReadFile(filepath.Join(out, e.Name()))
		if rerr != nil {
			t.Fatalf("could not read %s: %v", e.Name(), rerr)
		}
		if !bytes.Equal(b, payload) {
			t.Fatalf("%s content is corrupt (%d bytes)", e.Name(), len(b))
		}
	}
}

// Downloads running at the same time must not mix up while writing into separate folders.
func TestConcurrentDownloadsAcrossDirectories(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	const dirs, perDir = 8, 8
	var wg sync.WaitGroup
	for di := 0; di < dirs; di++ {
		for fi := 0; fi < perDir; fi++ {
			wg.Add(1)
			go func(di, fi int) {
				defer wg.Done()
				it := testItem(srv.URL+"/data.bin", "same.bin")
				it.Dir = fmt.Sprintf("album-%d", di)
				it.Index = fi
				it.SourcePage = fmt.Sprintf("https://example.test/u/%d-%d", di, fi)
				it.SHA256 = payloadSHA()
				if _, err := d.Download(context.Background(), out, it); err != nil {
					t.Errorf("album-%d/%d: %v", di, fi, err)
				}
			}(di, fi)
		}
	}
	wg.Wait()

	for di := 0; di < dirs; di++ {
		dir := filepath.Join(out, fmt.Sprintf("album-%d", di))
		entries := ownEntries(t, dir)
		if len(entries) != perDir {
			t.Errorf("%d files in %s, want %d", len(entries), dir, perDir)
		}
	}
}

// Concurrent cancellation: every download must have either completed or left
// a consistent .part. A partial file must NEVER sit under the final name.
func TestConcurrentCancelLeavesNoFinalPartialFiles(t *testing.T) {
	release := make(chan struct{})
	srv := slowServer(t, 8000, release)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	ctx, cancel := context.WithCancel(context.Background())
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			it := testItem(srv.URL+"/data.bin", fmt.Sprintf("canceled-%02d.bin", i))
			it.Index = i
			it.SHA256 = payloadSHA()
			_, _ = d.Download(ctx, out, it)
		}(i)
	}
	cancel()
	wg.Wait()
	close(release)

	entries := ownEntries(t, out)
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) == ".bin" {
			// A file under the final name must be complete.
			b, rerr := os.ReadFile(filepath.Join(out, name))
			if rerr != nil {
				t.Fatalf("%s: %v", name, rerr)
			}
			if !bytes.Equal(b, payload) {
				t.Fatalf("%s is under the final name but incomplete (%d bytes)", name, len(b))
			}
		}
	}
}

// The Downloader must not download one item twice: the second call must fall
// into the "already exists" branch and return the same path.
func TestDownloadTwiceReturnsSamePath(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)

	it := testItem(srv.URL+"/data.bin", "one.bin")
	it.SHA256 = payloadSHA()

	d := &Downloader{Client: srv.Client()}
	r1, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatal(err)
	}
	// A second call on the same Downloader produces a new name because of
	// claim(); that is correct (two different items may resolve to the same
	// name). A new Downloader targets the same name and falls into the
	// "already exists" branch.
	d2 := &Downloader{Client: srv.Client()}
	r2, err := d2.Download(context.Background(), out, it)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Path != r2.Path {
		t.Fatalf("the second run returned a different path:\n%s\n%s", r1.Path, r2.Path)
	}
	entries := ownEntries(t, out)
	if len(entries) != 1 {
		t.Fatalf("%d files present, want 1", len(entries))
	}
}
