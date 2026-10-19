package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/browser"
	"github.com/mustafaberatdemirci/siphon/internal/instance"
	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// maxBrowserLinks bounds one request from the browser.
const maxBrowserLinks = 2000

// runBrowserHost is the native messaging host the Siphon extension talks to:
// the browser starts this program, writes requests on its stdin and reads
// the answers on its stdout, so nothing else may be printed there. Links go
// to the running Siphon window (started if needed), which opens its Add
// dialog with them; nothing is queued without the user pressing Add.
func runBrowserHost() int {
	dir := run.StateDir()
	var rs []site.Resolver // built when a page's links first need sorting
	h := hostHandler{
		forward: func(links []string) error { return forwardLinks(dir, links) },
		recognized: func(u string) bool {
			if rs == nil {
				_, r, err := run.Setup(run.Events{}, "", nil, nil, nil)
				if err != nil {
					return false
				}
				rs = r
			}
			return run.Recognized(rs, u)
		},
	}
	if err := browser.Serve(os.Stdin, os.Stdout, h.handle); err != nil {
		fmt.Fprintf(os.Stderr, "siphon browser host: %v\n", err) // the browser's log
		return 1
	}
	return 0
}

type hostHandler struct {
	forward    func(links []string) error
	recognized func(u string) bool
}

func (h hostHandler) handle(req browser.Request) browser.Response {
	switch req.Type {
	case "ping":
		return browser.Response{OK: true, Version: version}
	case "add":
		links := cleanLinks(req.Links)
		if req.Page {
			// A whole page's links: only what a supported site recognizes.
			kept := links[:0]
			for _, l := range links {
				if h.recognized(l) {
					kept = append(kept, l)
				}
			}
			links = kept
		}
		if len(links) == 0 {
			return browser.Response{OK: true}
		}
		if err := h.forward(links); err != nil {
			return browser.Response{Error: err.Error()}
		}
		return browser.Response{OK: true, Added: len(links)}
	}
	return browser.Response{Error: fmt.Sprintf("unknown request %q", req.Type)}
}

// cleanLinks keeps web links (and tool-prefixed ones), each once, at most
// maxBrowserLinks of them.
func cleanLinks(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range in {
		l = strings.TrimSpace(l)
		low := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(l, site.ViaYtDlp), site.ViaGalleryDL))
		if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") || seen[l] || strings.ContainsAny(l, "\r\n") {
			continue
		}
		seen[l] = true
		out = append(out, l)
		if len(out) == maxBrowserLinks {
			break
		}
	}
	return out
}

// startWait is how long a Siphon window started for the browser has to come
// up and take the links.
var startWait = 20 * time.Second

// forwardLinks hands links to the running Siphon window, starting it first
// if none is running.
func forwardLinks(dir string, links []string) error {
	err := instance.Add(dir, links)
	if !errors.Is(err, instance.ErrNotRunning) {
		return err
	}
	gui := guiPath()
	if gui == "" {
		return errors.New("Siphon's window program wasn't found next to the command line program")
	}
	if err := browser.StartDetached(gui); err != nil {
		return fmt.Errorf("could not start Siphon: %w", err)
	}
	for deadline := time.Now().Add(startWait); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if err := instance.Add(dir, links); err == nil {
			return nil
		}
	}
	return errors.New("Siphon was started but didn't take the links in time")
}

// guiPath is the window program shipped next to this one: siphon-gui(.exe),
// or inside Siphon.app on macOS.
func guiPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	cands := []string{filepath.Join(dir, "siphon-gui")}
	switch runtime.GOOS {
	case "windows":
		cands = []string{filepath.Join(dir, "siphon-gui.exe")}
	case "darwin":
		cands = append(cands, filepath.Join(dir, "Siphon.app", "Contents", "MacOS", "Siphon"))
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}
