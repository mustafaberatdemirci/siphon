// Package external runs the download tools Siphon hands a link to when none
// of its own sites recognizes it: yt-dlp (video and audio from about 1,800
// sites) and gallery-dl (images and galleries from about 300 sites).
//
// They are used only if installed. Siphon doesn't bundle or download them:
// it stays a single file, and the user decides what runs on their machine.
// Everything the tools do is theirs (extraction, format choice, merging with
// ffmpeg); Siphon starts them, reads their progress and reports the result.
package external

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Tools holds the executables found; an empty path means "not installed".
type Tools struct {
	YtDlp     string
	GalleryDL string
	FFmpeg    string
}

// ErrUnsupported says the tool doesn't recognize the link.
var ErrUnsupported = errors.New("the tool doesn't support this link")

// Find locates the tools. For each: the path configured in sites.toml (if
// given it is the only place looked at: a wrong setting should show, not be
// papered over), then next to Siphon's own executable, then PATH. Cheap
// enough to call for every link, so a tool installed while Siphon runs is
// picked up.
func Find(configured map[string]string) Tools {
	return Tools{
		YtDlp:     lookup(configured["yt_dlp"], "yt-dlp"),
		GalleryDL: lookup(configured["gallery_dl"], "gallery-dl"),
		FFmpeg:    lookup(configured["ffmpeg"], "ffmpeg"),
	}
}

func lookup(configured, name string) string {
	if configured != "" {
		if isFile(configured) {
			return configured
		}
		return ""
	}
	if exe, err := os.Executable(); err == nil {
		for _, cand := range []string{name + ".exe", name} {
			if p := filepath.Join(filepath.Dir(exe), cand); isFile(p) {
				return p
			}
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// command prepares a tool run: UTF-8 output (both tools are Python; on
// Windows they would otherwise print file names in the console code page)
// and no console window popping up behind the GUI.
func command(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	// When the run is canceled (pause, cancel, quit) the process is killed;
	// don't hang on pipes a child process (ffmpeg) may still hold.
	cmd.WaitDelay = 5 * time.Second
	hideWindow(cmd)
	return cmd
}

// run starts cmd, hands every stdout line to onLine as it arrives and keeps
// the tail of stderr for error messages.
func run(cmd *exec.Cmd, onLine func(string)) (stderrTail string, err error) {
	var tail tailBuffer
	cmd.Stderr = &tail
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if onLine != nil {
			onLine(strings.TrimRight(sc.Text(), "\r"))
		}
	}
	_, _ = io.Copy(io.Discard, out)
	err = cmd.Wait()
	return tail.String(), err
}

// output runs cmd to the end and returns its stdout.
func output(cmd *exec.Cmd) (stdout []byte, stderrTail string, err error) {
	var buf bytes.Buffer
	stderrTail, err = run(cmd, func(l string) {
		buf.WriteString(l)
		buf.WriteByte('\n')
	})
	return buf.Bytes(), stderrTail, err
}

// tailBuffer keeps the last 8 KB written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - 8<<10; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// lastLineWith returns the last line of s containing marker (the tools
// report their errors as "ERROR: …" / "[error] …"), or the last non-empty
// line.
func lastLineWith(s, marker string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if strings.Contains(l, marker) {
			return l
		}
		if last == "" {
			last = l
		}
	}
	return last
}

// Lines describes, one line per tool, what was found and what is missing
// without it; doctor prints them.
func (t Tools) Lines() []string {
	row := func(name, path, without string) string {
		if path != "" {
			return fmt.Sprintf("  %-10s OK    %s", name, path)
		}
		return fmt.Sprintf("  %-10s -     not found: %s", name, without)
	}
	return []string{
		row("yt-dlp", t.YtDlp, "video and audio pages of other sites can't be downloaded"),
		row("gallery-dl", t.GalleryDL, "image and gallery pages of other sites can't be downloaded"),
		row("ffmpeg", t.FFmpeg, "videos whose picture and sound come separately can't be merged"),
	}
}
