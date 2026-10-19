package queue

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/netcheck"
)

// fakeNetwork answers network checks: hosts in blocked are blocked.
type fakeNetwork struct {
	mu      sync.Mutex
	blocked map[string]bool
	asked   []string
}

func (n *fakeNetwork) check(_ context.Context, host string) netcheck.Finding {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.asked = append(n.asked, host)
	if n.blocked[host] {
		return netcheck.Finding{Host: host, NameFiltered: true}
	}
	return netcheck.Finding{Host: host, Reached: true}
}

func (n *fakeNetwork) askedFor() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.asked...)
}

// timeoutAt is the error a request to rawURL gives when the network drops it.
func timeoutAt(rawURL string) error {
	return &url.Error{Op: "Get", URL: rawURL, Err: netDown}
}

// A link that can't be added because the network blocks its site says so,
// and the check isn't made again for every link of the same site.
func TestAddErrorSaysTheNetworkBlocksTheSite(t *testing.T) {
	f := newFakeSite(t)
	h := newHarness(t, f, "", 1)
	n := &fakeNetwork{blocked: map[string]bool{"www.mediafire.com": true}}
	h.e.opt.CheckNetwork = n.check
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		err := h.e.withNetworkNote(ctx, timeoutAt("https://www.mediafire.com/api/1.4/file/get_info.php?quick_key=x"), "https://www.mediafire.com/file/x")
		if !strings.Contains(err.Error(), "your network blocks www.mediafire.com") || !errors.Is(err, netDown) {
			t.Errorf("try %d: %v", i, err)
		}
	}
	if got := n.askedFor(); len(got) != 1 {
		t.Errorf("checked %d times (%v), want once", len(got), got)
	}

	// Not blocked: the error stays as it was. Not a network error: no check.
	plain := timeoutAt("https://pixeldrain.com/api/file/x")
	if err := h.e.withNetworkNote(ctx, plain, ""); err != plain {
		t.Errorf("a reachable site's error changed: %v", err)
	}
	notNet := errors.New("file deleted")
	if err := h.e.withNetworkNote(ctx, notNet, "https://cyberfile.me/x"); err != notNet {
		t.Errorf("a site's own answer changed: %v", err)
	}
	if got := n.askedFor(); len(got) != 2 {
		t.Errorf("checks: %v, want mediafire and pixeldrain only", got)
	}
}

// A queued job whose site the network blocks: the user is told once, in the
// background, while the job waits for the network as before.
func TestBlockedSiteIsToldWhileJobsWait(t *testing.T) {
	f := newFakeSite(t, "a.bin")
	h := newHarness(t, f, "", 1)
	h.e.opt.NetRetryBase = 20 * time.Millisecond
	n := &fakeNetwork{blocked: map[string]bool{"cyberfile.me": true}}
	h.e.opt.CheckNetwork = n.check
	var mu sync.Mutex
	var notices []string
	h.e.opt.OnNotice = func(s string) { mu.Lock(); notices = append(notices, s); mu.Unlock() }
	if _, err := h.e.Add(context.Background(), "file://a.bin", h.out); err != nil {
		t.Fatal(err)
	}
	forgetItems(h.e)
	h.e.workers["fake"].Resolver.(*fakeResolver).failNextResolveOne(3, timeoutAt("https://cyberfile.me/kwNO"))
	h.start()
	defer h.stop()
	h.waitAll(StateDone, 10*time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		told := 0
		for _, s := range notices {
			if strings.Contains(s, "your network blocks cyberfile.me") {
				told++
			}
		}
		mu.Unlock()
		if told == 1 {
			break
		}
		if told > 1 || time.Now().After(deadline) {
			t.Fatalf("told %d times, want once: %v", told, notices)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
