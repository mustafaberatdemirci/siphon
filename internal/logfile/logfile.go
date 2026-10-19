// Package logfile is the window version's log: a file that keeps the last
// megabyte or so of what the engine said (one older file besides it), with
// what could identify the user or open their links taken out before it is
// written. A user reporting a problem copies its end with Help → Copy
// diagnostic info; nothing leaves the computer by itself.
package logfile

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultMax is the size at which the log moves to <name>.1 and starts over.
const DefaultMax = 1 << 20

// File is a log file. Its methods are safe for concurrent use; a nil *File
// writes nowhere.
type File struct {
	path string
	max  int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open opens (or creates) the log at path, readable by the user only.
func Open(path string, max int64) (*File, error) {
	if max <= 0 {
		max = DefaultMax
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &File{path: path, max: max}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *File) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

// Path is where the log is.
func (l *File) Path() string { return l.path }

// Printf writes one line, stamped with the time and redacted.
func (l *File) Printf(format string, a ...any) {
	if l == nil {
		return
	}
	msg := strings.TrimRight(fmt.Sprintf(format, a...), "\r\n")
	// One entry, one line: a multi-line error would read as several entries.
	msg = strings.ReplaceAll(strings.ReplaceAll(msg, "\r\n", "\n"), "\n", " | ")
	line := time.Now().Format("2006-01-02 15:04:05 ") + Redact(msg) + "\n"

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	if l.size+int64(len(line)) > l.max && l.size > 0 {
		l.rotate()
		if l.f == nil {
			return
		}
	}
	n, _ := l.f.WriteString(line)
	l.size += int64(n)
}

// rotate, under the lock: the log becomes <name>.1 (the one before goes).
func (l *File) rotate() {
	l.f.Close()
	l.f = nil
	old := l.path + ".1"
	_ = os.Remove(old)
	if err := os.Rename(l.path, old); err != nil {
		// Can't move it (another program has it open): start it over instead
		// of growing without end.
		_ = os.Truncate(l.path, 0)
	}
	_ = l.open()
}

// Close closes the file; later lines are dropped.
func (l *File) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Tail returns the log's last n lines, the older file's included when the
// current one has fewer.
func (l *File) Tail(n int) []string {
	if l == nil || n <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	lines := readLines(l.path)
	if len(lines) < n {
		lines = append(readLines(l.path+".1"), lines...)
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func readLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

// urlPattern finds links in a line: the scheme up to the first space or quote.
var urlPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)

// Redact takes out of a line what could open the user's links or name them:
// a link's fragment (mega's key), its query values (signed download links,
// tokens, passwords), a user name and password in it, and the user's home
// folder in paths.
func Redact(s string) string {
	s = urlPattern.ReplaceAllStringFunc(s, redactURL)
	if home := homeDir(); home != "" {
		s = replaceFold(s, home, "~")
	}
	return s
}

func redactURL(u string) string {
	// Punctuation after a link belongs to the sentence ("<url>: HTTP 503").
	trimmed := strings.TrimRight(u, ".,:;)]")
	tail := u[len(trimmed):]
	u = trimmed
	if i := strings.IndexByte(u, '#'); i >= 0 && i < len(u)-1 {
		u = u[:i] + "#…"
	}
	if i := strings.IndexByte(u, '?'); i >= 0 {
		end := len(u)
		if j := strings.IndexByte(u[i:], '#'); j >= 0 {
			end = i + j
		}
		parts := strings.Split(u[i+1:end], "&")
		for k, p := range parts {
			if eq := strings.IndexByte(p, '='); eq >= 0 && eq < len(p)-1 {
				parts[k] = p[:eq+1] + "…"
			}
		}
		u = u[:i+1] + strings.Join(parts, "&") + u[end:]
	}
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		host := rest
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			host = rest[:j]
		}
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			u = u[:i+3] + "…@" + rest[at+1:]
		}
	}
	return u + tail
}

// homeDir is the user's home folder; a variable for tests.
var homeDir = func() string {
	h, err := os.UserHomeDir()
	if err != nil || len(h) < 4 { // never "/" or "C:\"
		return ""
	}
	return h
}

// replaceFold replaces old in s, ignoring case on Windows-style paths (the
// same folder is written C:\Users and c:\users).
func replaceFold(s, old, repl string) string {
	if old == "" {
		return s
	}
	lower, lowOld := strings.ToLower(s), strings.ToLower(old)
	if len(lower) != len(s) || len(lowOld) != len(old) {
		// A letter whose lower case is longer (İ): the offsets wouldn't match.
		return strings.ReplaceAll(s, old, repl)
	}
	var b strings.Builder
	for {
		i := strings.Index(lower, lowOld)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(repl)
		s, lower = s[i+len(old):], lower[i+len(old):]
	}
}
