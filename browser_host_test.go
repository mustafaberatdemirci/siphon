package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/browser"
	"github.com/mustafaberatdemirci/siphon/internal/instance"
)

func TestBrowserHostHandle(t *testing.T) {
	var forwarded [][]string
	h := hostHandler{
		forward: func(links []string) error {
			forwarded = append(forwarded, append([]string(nil), links...))
			return nil
		},
		recognized: func(u string) bool { return strings.Contains(u, "mega.nz") },
	}
	if r := h.handle(browser.Request{Type: "ping"}); !r.OK || r.Version == "" {
		t.Errorf("ping: %+v", r)
	}

	// A link the user chose goes as it is, whatever the site.
	r := h.handle(browser.Request{Type: "add", Links: []string{
		" https://example.com/a.zip ", "https://example.com/a.zip", "javascript:alert(1)", "ftp://x/y", "yt-dlp:https://v.example/1",
	}})
	if !r.OK || r.Added != 2 || strings.Join(forwarded[0], " ") != "https://example.com/a.zip yt-dlp:https://v.example/1" {
		t.Errorf("add: %+v, forwarded %q", r, forwarded)
	}

	// A page's links: only what a supported site recognizes.
	r = h.handle(browser.Request{Type: "add", Page: true, Links: []string{"https://news.example/x", "https://mega.nz/file/AbCd#k"}})
	if !r.OK || r.Added != 1 || len(forwarded) != 2 || forwarded[1][0] != "https://mega.nz/file/AbCd#k" {
		t.Errorf("page: %+v, forwarded %q", r, forwarded)
	}

	// Nothing left: nothing is forwarded, and that isn't an error.
	if r := h.handle(browser.Request{Type: "add", Page: true, Links: []string{"https://news.example/x"}}); !r.OK || r.Added != 0 || len(forwarded) != 2 {
		t.Errorf("empty page: %+v", r)
	}

	h.forward = func([]string) error { return errors.New("no window") }
	if r := h.handle(browser.Request{Type: "add", Links: []string{"https://example.com/a.zip"}}); r.OK || r.Error != "no window" {
		t.Errorf("failing forward: %+v", r)
	}
	if r := h.handle(browser.Request{Type: "delete-everything"}); r.OK || r.Error == "" {
		t.Errorf("unknown request: %+v", r)
	}
}

// The links reach the running Siphon window; with none running and no window
// program to start, the browser is told so.
func TestForwardLinks(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var got []string
	release, _ := instance.Claim(dir, instance.Handlers{Add: func(l []string) {
		mu.Lock()
		got = append(got, l...)
		mu.Unlock()
	}})
	if err := forwardLinks(dir, []string{"https://mega.nz/file/AbCd#k"}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 1 {
			break
		}
	}
	mu.Lock()
	if len(got) != 1 {
		t.Errorf("got %q", got)
	}
	mu.Unlock()
	release()

	// The test binary has no window program next to it.
	if err := forwardLinks(t.TempDir(), []string{"https://x.test/a"}); err == nil || !strings.Contains(err.Error(), "wasn't found") {
		t.Errorf("no window program: %v", err)
	}
}
