// Package instance keeps one Siphon window running at a time and lets other
// programs hand it work.
//
// Closing the window only hides it, so launching the exe again is an easy
// mistake, and two instances would drive the same queue file and download
// into the same .part files. The running instance listens on a loopback port
// and writes the port into instance.port next to queue.json; a new launch
// connects and asks it to show its window, then exits.
//
// The same channel takes links: the browser integration (siphon's native
// messaging host) and a launch with links as arguments hand them to the
// running window, which opens the Add dialog with them. Adding needs a key
// the instance writes into instance.port, readable by the user only:
// finding the port isn't enough to queue anything.
package instance

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	fileName    = "instance.port"
	showRequest = "siphon-show"
	addRequest  = "siphon-add"
	reply       = "siphon-ok"

	// Bounds of an add request: plenty for a page full of links.
	maxLinks    = 10000
	maxLinkSize = 64 << 10
)

// ErrNotRunning says no Siphon window answered.
var ErrNotRunning = errors.New("Siphon isn't running")

// Handlers are what the running instance does when asked. Both are called
// from the listener's goroutine; nil ones are skipped.
type Handlers struct {
	Show func()
	Add  func(links []string)
}

// Claim returns running=true if another instance answered (it has been asked
// to show its window; this one should exit). Otherwise this process becomes
// the instance: h is called whenever a later launch or the browser asks, and
// release stops listening. If dir is empty or the listener can't be set up,
// there is no guard (release is a no-op, running false).
func Claim(dir string, h Handlers) (release func(), running bool) {
	noop := func() {}
	if dir == "" {
		return noop, false
	}
	path := filepath.Join(dir, fileName)
	if Show(dir) {
		return noop, true
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return noop, false
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		ln.Close()
		return noop, false
	}
	token := hex.EncodeToString(key)
	_ = os.MkdirAll(dir, 0o755)
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	if err := os.WriteFile(path, []byte(port+"\n"+token+"\n"), 0o600); err != nil {
		ln.Close()
		return noop, false
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // closed
			}
			go serve(c, token, h)
		}
	}()
	return func() {
		ln.Close()
		// Only remove the file if it still points at us.
		if p, _, err := readFile(path); err == nil && p == port {
			_ = os.Remove(path)
		}
	}, false
}

func serve(c net.Conn, token string, h Handlers) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReaderSize(c, 4096)
	first, err := readLine(r)
	if err != nil {
		return
	}
	switch {
	case first == showRequest:
		_, _ = c.Write([]byte(reply + "\n"))
		if h.Show != nil {
			h.Show()
		}
	case strings.HasPrefix(first, addRequest+" "):
		given := strings.TrimPrefix(first, addRequest+" ")
		if subtle.ConstantTimeCompare([]byte(given), []byte(token)) != 1 {
			return
		}
		var links []string
		for len(links) < maxLinks {
			line, err := readLine(r)
			if err != nil {
				return
			}
			if line == "" {
				break
			}
			links = append(links, line)
		}
		_, _ = c.Write([]byte(reply + "\n"))
		if h.Add != nil && len(links) > 0 {
			h.Add(links)
		}
	}
}

// readLine reads one line of at most maxLinkSize bytes, without its ending.
func readLine(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		part, isPrefix, err := r.ReadLine()
		if err != nil {
			return "", err
		}
		b.Write(part)
		if b.Len() > maxLinkSize {
			return "", errors.New("line too long")
		}
		if !isPrefix {
			return strings.TrimSpace(b.String()), nil
		}
	}
}

// readFile reads instance.port: the port, then the key (missing in files
// written before links could be handed over).
func readFile(path string) (port, token string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	lines := strings.Fields(string(b))
	if len(lines) == 0 {
		return "", "", errors.New("empty port file")
	}
	if len(lines) > 1 {
		token = lines[1]
	}
	return lines[0], token, nil
}

// Show asks the instance recorded in dir to show its window. True only if
// Siphon answered.
func Show(dir string) bool {
	port, _, err := readFile(filepath.Join(dir, fileName))
	if err != nil {
		return false
	}
	return ask(port, showRequest+"\n")
}

// Add hands links to the running instance, which opens its Add dialog with
// them. ErrNotRunning if no instance answered. Blank lines are dropped.
func Add(dir string, links []string) error {
	port, token, err := readFile(filepath.Join(dir, fileName))
	if err != nil || token == "" {
		return ErrNotRunning
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", addRequest, token)
	n := 0
	for _, l := range links {
		l = strings.TrimSpace(l)
		if l == "" || strings.ContainsAny(l, "\r\n") || len(l) > maxLinkSize {
			continue
		}
		if n++; n > maxLinks {
			break
		}
		b.WriteString(l + "\n")
	}
	b.WriteString("\n")
	if !ask(port, b.String()) {
		return ErrNotRunning
	}
	return nil
}

// ask sends msg to the port and reports whether Siphon replied.
func ask(port, msg string) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
	if err != nil {
		return false // stale file: the instance that wrote it is gone
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		return false
	}
	line, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line) == reply
}
