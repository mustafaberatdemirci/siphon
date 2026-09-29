package run

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/dl"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/store"
	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

// tempDir delegates to the shared helper (the Windows file lock problem).
func tempDir(t *testing.T) string {
	t.Helper()
	return testutil.TempDir(t)
}

// fakeResolver calls yield and returns the given items. It is used instead
// of a real resolver because what is tested is run.go's EVENT ORDER, not the
// site protocol.
type fakeResolver struct {
	items []site.Item
}

func (f *fakeResolver) Match(string) bool { return true }

func (f *fakeResolver) Resolve(ctx context.Context, u string, yield func(site.Item) error) ([]site.ItemError, error) {
	for _, it := range f.items {
		if err := yield(it); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (f *fakeResolver) ResolveOne(context.Context, string) (site.Item, error) {
	return site.Item{}, fmt.Errorf("unexpected re-resolution")
}

func (f *fakeResolver) Diagnose(context.Context) ([]site.LayerResult, error) { return nil, nil }

// newRunCtx sets up a ready run context for runOne: the real downloader, the
// real ledger, a fake resolver.
func newRunCtx(t *testing.T, outDir string, r site.Resolver, ev Events) runCtx {
	t.Helper()
	ledger, err := store.Open(outDir)
	if err != nil {
		t.Fatalf("could not open the ledger: %v", err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	return runCtx{
		url:      "https://fake.test/a/1",
		resolver: r,
		cfg:      site.SiteConfig{Name: "fake"}.WithDefaults(),
		client:   http.DefaultClient,
		ledger:   ledger,
		opt:      Options{OutDir: outDir},
		ev:       ev,
		inFlight: 4,
	}
}

func fakeItems(base string, n int) []site.Item {
	items := make([]site.Item, n)
	for i := range items {
		items[i] = site.Item{
			URL:        fmt.Sprintf("%s/f/%d", base, i),
			SourcePage: fmt.Sprintf("%s/p/%d", base, i),
			Filename:   fmt.Sprintf("d%d.bin", i),
			Size:       -1,
			Index:      i,
		}
	}
	return items
}

// THIS WAS THE REAL BUG: the UI counted the total number of files from
// URLResolved, and that event fires when ALL items of the URL are done. As a
// result the progress bar stayed at zero for the whole run.
//
// This test verifies the contract at its source: ALL ItemQueued events must
// have arrived while NO file has finished. The server holds the body until
// released, so the result doesn't depend on timing.
func TestItemQueuedArrivesBeforeAnyDownloadFinishes(t *testing.T) {
	outDir := tempDir(t)

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	const n = 3
	var (
		mu          sync.Mutex
		queued      int
		finished    int
		doneAtQueue []int // how many files had finished at each ItemQueued
	)
	allQueued := make(chan struct{})

	ev := Events{
		ItemQueued: func(site.Item) {
			mu.Lock()
			queued++
			doneAtQueue = append(doneAtQueue, finished)
			q := queued
			mu.Unlock()
			if q == n {
				close(allQueued)
			}
		},
		ItemDone: func(site.Item, dl.Result) {
			mu.Lock()
			finished++
			mu.Unlock()
		},
	}

	rc := newRunCtx(t, outDir, &fakeResolver{items: fakeItems(srv.URL, n)}, ev)

	go func() {
		// Let the downloads finish only AFTER every item is queued.
		select {
		case <-allQueued:
		case <-time.After(10 * time.Second):
			// Release anyway so a deadlock isn't left to the test timeout.
		}
		close(release)
	}()

	res := runOne(context.Background(), rc)

	if res.resolveErr != nil {
		t.Fatalf("resolution error: %v", res.resolveErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if queued != n {
		t.Fatalf("ItemQueued arrived %d times, want %d", queued, n)
	}
	for i, d := range doneAtQueue {
		if d != 0 {
			t.Errorf("when ItemQueued #%d arrived %d files had finished; queue notifications must come before work finishes", i+1, d)
		}
	}
	if res.done != n {
		t.Fatalf("downloaded files = %d, want %d", res.done, n)
	}
}

// quotaResolver classifies 509 as quota (like mega).
type quotaResolver struct{ fakeResolver }

func (quotaResolver) ClassifyStatus(resp *http.Response, _ []byte) error {
	if resp.StatusCode == 509 {
		return &site.QuotaError{Wait: time.Hour, Err: site.Errorf(site.LayerCDN, "fake", "quota exceeded (fake)")}
	}
	return nil
}

// When the quota runs out the album must STOP: in a 373-file mega folder
// every remaining item gets the same 509 in turn, none downloads, only
// requests are spent. The server counts how many requests it saw; no new
// item may start after the 509.
func TestQuotaHaltsTheAlbum(t *testing.T) {
	outDir := tempDir(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "Bandwidth Limit Exceeded", 509)
	}))
	defer srv.Close()

	const n = 40
	rc := newRunCtx(t, outDir, &quotaResolver{fakeResolver{items: fakeItems(srv.URL, n)}}, Events{})
	rc.inFlight = 1 // sequential: so it's clear how many items were tried after the first 509
	res := runOne(context.Background(), rc)

	if !res.halted {
		t.Fatal("the quota did not halt the album")
	}
	if h := hits.Load(); h > 2 {
		t.Errorf("%d more requests were sent after the 509; the album should have stopped at the first quota", h-1)
	}
	if res.failed != 1 {
		t.Errorf("failed = %d, want 1 (the item that hit the quota)", res.failed)
	}
}

// Resolvers must be built with a client that HAS TIMEOUTS. cfg.HTTPClient
// used to never be set in production and API/page requests went through
// http.DefaultClient (no response-header timeout): a server that doesn't
// answer locked the UI's "Add" forever.
func TestSetupGivesResolversAClient(t *testing.T) {
	cfgs, _, err := Setup(Events{}, "", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cfgs {
		if c.HTTPClient == nil || c.HTTPClient == http.DefaultClient {
			t.Errorf("%s: resolver client missing or the default without timeouts", c.Name)
		}
	}

	// A given client is shared (the same connection pool as the downloader).
	shared := &http.Client{}
	cfgs, _, err = Setup(Events{}, "", nil, nil, shared)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cfgs {
		if c.HTTPClient != shared {
			t.Errorf("%s: the given client was not used", c.Name)
		}
	}
}

// ctxQuotaResolver checks the context before every item like real resolvers
// do, and counts how many items it produced.
type ctxQuotaResolver struct {
	quotaResolver
	yielded atomic.Int32
}

func (r *ctxQuotaResolver) Resolve(ctx context.Context, u string, yield func(site.Item) error) ([]site.ItemError, error) {
	for _, it := range r.items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.yielded.Add(1)
		if err := yield(it); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// When the album stops, RESOLUTION must stop too. On bunkr every item is an
// API call; with a resolver called on a context that isn't canceled,
// pointless requests were sent for the hundreds of files left after the quota.
func TestQuotaHaltStopsResolution(t *testing.T) {
	outDir := tempDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Bandwidth Limit Exceeded", 509)
	}))
	defer srv.Close()

	const n = 40
	r := &ctxQuotaResolver{quotaResolver: quotaResolver{fakeResolver{items: fakeItems(srv.URL, n)}}}
	rc := newRunCtx(t, outDir, r, Events{})
	rc.inFlight = 1
	res := runOne(context.Background(), rc)

	if !res.halted {
		t.Fatal("the quota did not halt the album")
	}
	if got := r.yielded.Load(); got > 3 {
		t.Errorf("%d more items were resolved after stopping; resolution should have been cut at the first quota", got-1)
	}
}

// -on-quota: when the quota runs out the command runs, the SAME URL is run
// again and the ledger skips what was downloaded. The server serves the first
// N requests, then returns 509, and opens up again once the command runs
// (marker file).
func TestOnQuotaCommandThenRetriesSameURL(t *testing.T) {
	outDir := tempDir(t)
	marker := filepath.Join(tempDir(t), "vpn.txt")
	var served atomic.Int32
	quotaOn := atomic.Bool{}
	quotaOn.Store(false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(marker); err == nil {
			quotaOn.Store(false) // "the VPN switched"
		}
		if served.Load() >= 2 && !quotaOn.Load() {
			// 2 files downloaded; if the command hasn't run, fill the quota.
			if _, err := os.Stat(marker); err != nil {
				quotaOn.Store(true)
			}
		}
		if quotaOn.Load() {
			http.Error(w, "Bandwidth Limit Exceeded", 509)
			return
		}
		served.Add(1)
		w.Header().Set("Content-Length", "4")
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	const n = 5
	r := &quotaResolver{fakeResolver{items: fakeItems(srv.URL, n)}}
	rc := newRunCtx(t, outDir, r, Events{})
	rc.inFlight = 1
	rc.opt.OnQuota = `echo switched> "` + marker + `"`
	rc.opt.QuotaRetryDelay = 10 * time.Millisecond

	var sum Summary
	res := runURL(context.Background(), rc, &sum)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the quota command did not run")
	}
	if sum.Done != n {
		t.Fatalf("total downloaded %d, want %d (the second round should download the rest)", sum.Done, n)
	}
	if sum.Skipped != 2 {
		t.Errorf("in the second round the ledger skipped %d files, want 2", sum.Skipped)
	}
	if res.quota {
		t.Error("the last round ended in quota again")
	}
}

// Every queued item MUST close with Done, Failed or Skipped; otherwise the
// progress bar never reaches one hundred percent.
func TestEveryQueuedItemIsAccountedFor(t *testing.T) {
	outDir := tempDir(t)

	// The third file returns 404: accounting must hold with mixed results too.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/f/2" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	const n = 4
	var mu sync.Mutex
	counts := map[string]int{}
	ev := Events{
		ItemQueued:  func(site.Item) { mu.Lock(); counts["queued"]++; mu.Unlock() },
		ItemDone:    func(site.Item, dl.Result) { mu.Lock(); counts["done"]++; mu.Unlock() },
		ItemFailed:  func(site.Item, error) { mu.Lock(); counts["failed"]++; mu.Unlock() },
		ItemSkipped: func(site.Item, store.Entry) { mu.Lock(); counts["skipped"]++; mu.Unlock() },
	}

	rc := newRunCtx(t, outDir, &fakeResolver{items: fakeItems(srv.URL, n)}, ev)
	rc.cfg.MaxRetries = 1 // don't wait for repeated retries on the 404
	_ = runOne(context.Background(), rc)

	mu.Lock()
	defer mu.Unlock()
	if counts["queued"] != n {
		t.Fatalf("queued = %d, want %d", counts["queued"], n)
	}
	closed := counts["done"] + counts["failed"] + counts["skipped"]
	if closed != n {
		t.Fatalf("closed items = %d (done=%d failed=%d skipped=%d), want %d: "+
			"an item that never closes keeps the progress bar from filling",
			closed, counts["done"], counts["failed"], counts["skipped"], n)
	}
	if counts["failed"] == 0 {
		t.Error("the file returning 404 was not counted as failed")
	}
}

// In the second run the files are already in the ledger: every item must
// still be queued and close with Skipped. Otherwise the bar stays empty in the
// "everything is already downloaded" case.
func TestSecondRunQueuesAndSkipsEveryItem(t *testing.T) {
	outDir := tempDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	const n = 3
	items := fakeItems(srv.URL, n)

	first := runOne(context.Background(), newRunCtx(t, outDir, &fakeResolver{items: items}, Events{}))
	if first.done != n {
		t.Fatalf("downloaded in the first run = %d, want %d", first.done, n)
	}
	for i := 0; i < n; i++ {
		if _, err := os.Stat(filepath.Join(outDir, fmt.Sprintf("d%d.bin", i))); err != nil {
			t.Fatalf("file missing: %v", err)
		}
	}

	var mu sync.Mutex
	var queued, skipped int
	ev := Events{
		ItemQueued:  func(site.Item) { mu.Lock(); queued++; mu.Unlock() },
		ItemSkipped: func(site.Item, store.Entry) { mu.Lock(); skipped++; mu.Unlock() },
	}
	second := runOne(context.Background(), newRunCtx(t, outDir, &fakeResolver{items: items}, ev))

	mu.Lock()
	defer mu.Unlock()
	if queued != n {
		t.Fatalf("queued in the second run = %d, want %d", queued, n)
	}
	if skipped != n {
		t.Fatalf("skipped = %d, want %d", skipped, n)
	}
	if second.skipped != n {
		t.Fatalf("summary skipped = %d, want %d", second.skipped, n)
	}
}
