package testutil

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fake yt-dlp and gallery-dl for tests: the test binary itself, started
// again with FakeToolEnv set, plays the tool. Tests stay offline and don't
// depend on (or get disturbed by) a real tool on the machine.
//
// A package using it calls FakeToolMain first thing in its TestMain, and
// points the tool paths at FakeToolPath(t).
//
// Which tool it plays is read from the arguments (yt-dlp is always called
// with -J or --progress-template, gallery-dl with --simulate or -d). What it
// answers depends on the link's path, whatever the host:
//
//	yt-dlp      /watch?v=<id>  a video titled "Video <id>"
//	            /playlist      "My list" with videos one and two
//	            /private       fails: "Private video"
//	            /image         fails: "not a video" (the site is known)
//	            /split         reports a file that isn't on disk
//	            anything else  "Unsupported URL"
//	gallery-dl  /album         two images, a.jpg and b.jpg
//	            /image         accepted by the probe
//	            anything else  "Unsupported URL"
//
// Every call's arguments are appended to the file named by FakeToolLogEnv,
// one JSON array per line, if set.
const (
	FakeToolEnv    = "SIPHON_FAKE_TOOL"
	FakeToolLogEnv = "SIPHON_FAKE_TOOL_LOG"
)

// FakeToolPath returns the executable to configure as the tool, and makes
// the child processes act as the fake.
func FakeToolPath(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(FakeToolEnv, "1")
	return exe
}

// FakeToolMain plays the tool and exits if this process was started as one.
func FakeToolMain() {
	if os.Getenv(FakeToolEnv) == "" {
		return
	}
	args := os.Args[1:]
	if log := os.Getenv(FakeToolLogEnv); log != "" {
		if f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			b, _ := json.Marshal(args)
			fmt.Fprintf(f, "%s\n", b)
			f.Close()
		}
	}
	os.Exit(fakeTool(args))
}

func fakeTool(args []string) int {
	link := ""
	if len(args) > 0 {
		link = args[len(args)-1]
	}
	has := func(flag string) bool {
		for _, a := range args {
			if a == flag {
				return true
			}
		}
		return false
	}
	after := func(flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	path := link
	if i := strings.Index(link, "://"); i >= 0 {
		path = link[i+3:]
		if j := strings.Index(path, "/"); j >= 0 {
			path = path[j:]
		}
	}

	switch {
	case has("-J"): // yt-dlp probe
		switch {
		case strings.HasPrefix(path, "/watch?v="):
			id := strings.TrimPrefix(path, "/watch?v=")
			printJSON(map[string]any{"_type": "video", "id": id, "title": "Video " + id, "webpage_url": link, "filesize_approx": 1000})
		case path == "/playlist":
			base := strings.TrimSuffix(link, "/playlist")
			printJSON(map[string]any{"_type": "playlist", "title": "My list", "entries": []any{
				map[string]any{"_type": "url", "url": base + "/watch?v=one", "title": "Video one", "id": "one"},
				map[string]any{"_type": "url", "url": base + "/watch?v=two", "title": "Video two", "id": "two"},
			}})
		case path == "/private":
			fmt.Fprintln(os.Stderr, "ERROR: [test] private: Private video. Sign in if you've been granted access")
			return 1
		case path == "/image":
			// MEASURED on imgur: yt-dlp claims the site, then fails.
			fmt.Fprintln(os.Stderr, "ERROR: [test] image: image is not a video or animated image")
			return 1
		default:
			fmt.Fprintln(os.Stderr, "ERROR: Unsupported URL: "+link)
			return 1
		}
		return 0

	case has("--progress-template"): // yt-dlp download
		dir := after("-P")
		if path == "/split" {
			fmt.Println("SIPHON-FILE " + filepath.Join(dir, "Split [s].webm"))
			return 0
		}
		if !strings.HasPrefix(path, "/watch?v=") {
			fmt.Fprintln(os.Stderr, "ERROR: Unsupported URL: "+link)
			return 1
		}
		id := strings.TrimPrefix(path, "/watch?v=")
		body := []byte(strings.Repeat("video "+id+" ", 100))
		for _, n := range []int{0, len(body) / 2, len(body)} {
			fmt.Printf("SIPHON-PROGRESS %d %d NA\n", n, len(body))
		}
		out := filepath.Join(dir, "Video "+id+" ["+id+"].mp4")
		if err := os.WriteFile(out, body, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR: "+err.Error())
			return 1
		}
		fmt.Println("SIPHON-FILE " + out)
		return 0

	case has("--simulate"): // gallery-dl probe
		if path != "/album" && path != "/image" {
			fmt.Fprintf(os.Stderr, "[gallery-dl][error] Unsupported URL '%s'\n", link)
			return 64
		}
		fmt.Println("# a.jpg")
		return 0

	case has("-d"): // gallery-dl download
		if path != "/album" {
			fmt.Fprintf(os.Stderr, "[gallery-dl][error] Unsupported URL '%s'\n", link)
			return 64
		}
		dir := filepath.Join(after("-d"), "pics", "album")
		_ = os.MkdirAll(dir, 0o755)
		for _, name := range []string{"a.jpg", "b.jpg"} {
			p := filepath.Join(dir, name)
			if _, err := os.Stat(p); err == nil {
				fmt.Println("# " + p) // already there: skipped
				continue
			}
			_ = os.WriteFile(p, []byte("image "+name), 0o644)
			fmt.Println(p)
		}
		return 0
	}
	fmt.Fprintln(os.Stderr, "fake tool: unexpected arguments", args)
	return 2
}

func printJSON(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}
