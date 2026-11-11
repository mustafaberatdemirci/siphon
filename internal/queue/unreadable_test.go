package queue

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

// A queue file that can't be read used to be replaced by the empty queue at
// the first save: a corrupt file, or a newer version's after a downgrade,
// and the whole list was gone. It is kept under another name.
func TestUnreadableStateFileIsKept(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt":       `{"version":1,"jobs":[{"id":"x"`,
		"newer version": `{"version":99,"jobs":[{"id":"x","site":"fake","state":"queued"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			statePath := filepath.Join(testutil.TempDir(t), "queue.json")
			if err := os.WriteFile(statePath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			f := newFakeSite(t, "a.bin")
			var logged []string
			e, err := New(Options{
				StatePath: statePath, Client: f.srv.Client(),
				Resolvers: []site.Resolver{&fakeResolver{f: f}},
				Configs:   []site.SiteConfig{site.SiteConfig{Name: "fake"}.WithDefaults()},
				Events:    runEvents(func(s string) { logged = append(logged, s) }),
			})
			if err != nil {
				t.Fatal(err)
			}
			kept := e.UnreadableStateFile()
			if kept == "" || kept == statePath {
				t.Fatalf("the unreadable file was not set aside (%q)", kept)
			}
			if len(logged) == 0 || !strings.Contains(logged[0], filepath.Base(kept)) {
				t.Errorf("the log doesn't say where the file went: %v", logged)
			}

			// The save that used to destroy it.
			e.flush()
			if got, err := os.ReadFile(kept); err != nil || string(got) != content {
				t.Errorf("the kept file changed: %q, %v", got, err)
			}
			data, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatalf("no new queue file: %v", err)
			}
			var sf stateFile
			if err := json.Unmarshal(data, &sf); err != nil || sf.Version != stateVersion {
				t.Errorf("the new queue file is not a queue: %v", err)
			}
		})
	}
}

// A queue file that reads fine is left alone.
func TestReadableStateFileIsNotSetAside(t *testing.T) {
	statePath := filepath.Join(testutil.TempDir(t), "queue.json")
	if err := save(statePath, nil); err != nil {
		t.Fatal(err)
	}
	f := newFakeSite(t, "a.bin")
	e, err := New(Options{
		StatePath: statePath, Client: f.srv.Client(),
		Resolvers: []site.Resolver{&fakeResolver{f: f}},
		Configs:   []site.SiteConfig{site.SiteConfig{Name: "fake"}.WithDefaults()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if kept := e.UnreadableStateFile(); kept != "" {
		t.Errorf("a readable queue file was set aside as %s", kept)
	}
}

// Saves from the timer and from shutdown wrote the same temp file at once.
func TestConcurrentFlushesLeaveAReadableFile(t *testing.T) {
	f := newFakeSite(t, "a.bin", "b.bin", "c.bin")
	statePath := filepath.Join(testutil.TempDir(t), "queue.json")
	h := newHarness(t, f, statePath, 1)
	if _, err := h.e.Add(context.Background(), "album://x", h.out); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.e.flush() }()
	}
	wg.Wait()
	jobs, err := load(statePath)
	if err != nil || len(jobs) != 3 {
		t.Fatalf("after concurrent saves: %d jobs, %v", len(jobs), err)
	}
}
