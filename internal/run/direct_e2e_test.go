package run

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/megacrypto"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/store"
)

// Real sites are asked before the plain-link fallback, whatever the order
// the resolvers are in; what no site recognizes goes to the fallback.
func TestPickAsksRealSitesBeforeTheFallback(t *testing.T) {
	cfgs, resolvers, err := Setup(Events{}, "", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Put the fallback first on purpose.
	for i, c := range cfgs {
		if c.Name == site.DirectName {
			resolvers[0], resolvers[i] = resolvers[i], resolvers[0]
			cfgs[0], cfgs[i] = cfgs[i], cfgs[0]
		}
	}
	fileKey := megacrypto.B64Encode(bytes.Repeat([]byte{7}, 32))
	for u, want := range map[string]string{
		"https://pixeldrain.com/u/abc123":        site.PixeldrainName,
		"https://bunkr.ws/a/xyz":                 site.BunkrName,
		"https://mega.nz/file/AbCd#" + fileKey:   site.MegaName,
		"https://example.com/files/setup.zip":    site.DirectName,
		"https://downloads.example.org/iso/x.7z": site.DirectName,
		"ftp://example.com/x":                    "",
		// A real site's host, in a form it doesn't support: NOT a plain
		// file. Downloading these would save a web page or ciphertext.
		"https://mega.nz/file/AbCd#truncatedkey":           "",
		"https://gfs270n172.userstorage.mega.co.nz/dl/xyz": "",
		"https://c3bc-b.cdn.cr/video.mp4":                  "",
		"https://www.pixeldrain.com/":                      "",
		"https://bunkr.si/":                                "",
	} {
		_, idx := Pick(resolvers, u)
		got := ""
		if idx >= 0 {
			got = cfgs[idx].Name
		}
		if got != want {
			t.Errorf("%s -> %q, want %q", u, got, want)
		}
	}
}

// fileServer serves one file with range support and an ETag, counting the
// range requests and the connections open at once.
type fileServer struct {
	srv    *httptest.Server
	body   []byte
	mu     sync.Mutex
	ranges map[string]bool
	inFly  atomic.Int32
	maxFly atomic.Int32
}

func newFileServer(t *testing.T, size int, contentType string) *fileServer {
	t.Helper()
	f := &fileServer{body: make([]byte, size), ranges: map[string]bool{}}
	_, _ = rand.Read(f.body)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.inFly.Add(1)
		defer f.inFly.Add(-1)
		for {
			m := f.maxFly.Load()
			if n <= m || f.maxFly.CompareAndSwap(m, n) {
				break
			}
		}
		f.mu.Lock()
		f.ranges[r.Header.Get("Range")] = true
		f.mu.Unlock()
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("Range") != "bytes=0-0" {
			time.Sleep(30 * time.Millisecond) // let the connections overlap
		}
		http.ServeContent(w, r, "", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), bytes.NewReader(f.body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// directRunCtx builds a run for a plain link with the EMBEDDED "direct"
// settings (sites.toml), as the real program would.
func directRunCtx(t *testing.T, outDir, link string, client *http.Client) runCtx {
	t.Helper()
	cfgs, resolvers, err := Setup(Events{}, "", nil, nil, client)
	if err != nil {
		t.Fatal(err)
	}
	r, idx := Pick(resolvers, link)
	if r == nil || cfgs[idx].Name != site.DirectName {
		t.Fatalf("%s wasn't given to the plain-link fallback", link)
	}
	ledger, err := store.Open(outDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	return runCtx{
		url: link, resolver: r, cfg: cfgs[idx], client: client,
		ledger: ledger, opt: Options{OutDir: outDir}, inFlight: 4,
	}
}

// A plain file link goes through the ordinary downloader: several
// connections for a large file, the right bytes on disk under the link's
// name, and the ledger skips it on the next run.
func TestDirectLinkEndToEnd(t *testing.T) {
	f := newFileServer(t, 9<<20, "application/octet-stream") // above the 8 MiB splitting threshold
	out := tempDir(t)
	link := f.srv.URL + "/releases/setup-1.2.bin"

	res := runOne(context.Background(), directRunCtx(t, out, link, f.srv.Client()))
	if res.resolveErr != nil || res.done != 1 {
		t.Fatalf("run: %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(out, "setup-1.2.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, f.body) {
		t.Fatal("the downloaded file is corrupt")
	}
	f.mu.Lock()
	chunks := len(f.ranges) - 1 // minus the first-byte probe
	f.mu.Unlock()
	if chunks < 2 || f.maxFly.Load() < 2 {
		t.Errorf("%d range requests, at most %d connections at once; a 9 MiB file should use several",
			chunks, f.maxFly.Load())
	}

	again := runOne(context.Background(), directRunCtx(t, out, link, f.srv.Client()))
	if again.skipped != 1 || again.done != 0 {
		t.Errorf("the second run didn't skip the file: %+v", again)
	}
}

// A link to a web page is refused at resolution: nothing is written, and
// the run reports why.
func TestDirectLinkToWebPageSavesNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>Sign in to download</html>")
	}))
	t.Cleanup(srv.Close)
	out := tempDir(t)
	res := runOne(context.Background(), directRunCtx(t, out, srv.URL+"/file/123", srv.Client()))
	if res.resolveErr == nil || res.done != 0 {
		t.Fatalf("a web page was accepted: %+v", res)
	}
	entries, _ := os.ReadDir(out)
	for _, e := range entries {
		if e.Name() != store.FileName {
			t.Errorf("%s was written for a web page link", e.Name())
		}
	}
}
