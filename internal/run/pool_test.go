package run

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

func TestSlotKeyIsThePoolOrTheHost(t *testing.T) {
	cfg := site.SiteConfig{Name: "fake", MaxConcurrent: 4,
		HostLimits: map[string]int{"*.slow.test": 1, "*.mid.test": 2}}.WithDefaults()
	w := NewWorker(&fakeResolver{}, cfg, http.DefaultClient, Events{})
	for u, want := range map[string]string{
		"https://a.slow.test/f/1":      "*.slow.test",
		"https://B.Slow.Test:8443/f/2": "*.slow.test",
		"https://x.mid.test/f/3":       "*.mid.test",
		"https://fast.test/f/4":        "fast.test",
		"https://a.b.slow.test/f/5":    "a.b.slow.test", // one label only, as everywhere
	} {
		if got := w.SlotKey(u); got != want {
			t.Errorf("SlotKey(%s) = %q, want %q", u, got, want)
		}
	}
	if got := w.SlotLimit("*.slow.test"); got != 1 {
		t.Errorf("the pool's limit = %d, want 1", got)
	}
	if got := w.SlotLimit("*.mid.test"); got != 2 {
		t.Errorf("the second pool's limit = %d, want 2", got)
	}
	if got := w.SlotLimit("fast.test"); got != 4 {
		t.Errorf("a plain host's limit = %d, want max_concurrent", got)
	}
}

// The command line waits for a pool's slot (it has nothing else to start);
// the queue asks without waiting and is told the pool is busy.
func TestPoolLimitHoldsFilesToTheirTurn(t *testing.T) {
	var inflight, most atomic.Int64
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		<-gate
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	cfg := site.SiteConfig{Name: "fake", MaxConcurrent: 4, MaxRetries: 1,
		HostLimits: map[string]int{"127.0.0.1": 1}}.WithDefaults()
	w := NewWorker(&fakeResolver{}, cfg, srv.Client(), Events{})
	out := tempDir(t)
	item := func(name string) site.Item {
		return site.Item{URL: srv.URL + "/" + name, SourcePage: "https://fake.test/f/" + name, Filename: name, Size: 4}
	}

	var wg sync.WaitGroup
	results := make([]Outcome, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0] = w.DownloadItem(context.Background(), out, nil, item("a.bin"), Events{})
	}()
	for inflight.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	if o := w.DownloadItemIfFree(context.Background(), out, nil, item("b.bin"), Events{}); o.Kind != OutcomeBusy {
		t.Errorf("with the pool's one slot taken, DownloadItemIfFree = %v, want OutcomeBusy", o.Kind)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		results[1] = w.DownloadItem(context.Background(), out, nil, item("c.bin"), Events{})
	}()
	time.Sleep(150 * time.Millisecond) // c.bin would be at the server by now if it didn't wait
	if got := most.Load(); got != 1 {
		t.Errorf("%d files of the pool at the server at once, want 1", got)
	}
	close(gate)
	wg.Wait()
	for i, o := range results {
		if o.Kind != OutcomeDone {
			t.Errorf("download %d: %v (%v)", i, o.Kind, o.Err)
		}
	}
	if got := most.Load(); got != 1 {
		t.Errorf("%d files of the pool at the server at once, want 1", got)
	}
}
