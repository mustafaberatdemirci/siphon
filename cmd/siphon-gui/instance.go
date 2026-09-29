package main

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Only one Siphon window runs at a time. Once closing the window only hides
// it, launching the exe again is an easy mistake, and two instances would
// drive the same queue file and download into the same .part files. A
// second launch asks the running instance to show its window, then exits.
//
// The running instance listens on a loopback port and writes the port into
// instance.port next to queue.json. A new launch connects, says showRequest
// and waits for showReply: the reply proves it reached Siphon and not some
// other program that took the port after a crash left the file behind.

const (
	instanceFile = "instance.port"
	showRequest  = "siphon-show"
	showReply    = "siphon-ok"
)

// claimInstance returns running=true if another instance answered (it has
// been asked to show its window; this one should exit). Otherwise this
// process becomes the instance: show is called whenever a later launch asks,
// and release stops listening. If dir is empty or the listener can't be set
// up, there is no guard (release is a no-op, running false).
func claimInstance(dir string, show func()) (release func(), running bool) {
	noop := func() {}
	if dir == "" {
		return noop, false
	}
	path := filepath.Join(dir, instanceFile)
	if askRunning(path) {
		return noop, true
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return noop, false
	}
	_ = os.MkdirAll(dir, 0o755)
	port := ln.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(path, []byte(strconv.Itoa(port)), 0o644); err != nil {
		ln.Close()
		return noop, false
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // closed
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(2 * time.Second))
				line, _ := bufio.NewReader(c).ReadString('\n')
				if strings.TrimSpace(line) != showRequest {
					return
				}
				_, _ = c.Write([]byte(showReply + "\n"))
				show()
			}(c)
		}
	}()
	return func() {
		ln.Close()
		// Only remove the file if it still points at us.
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(port) {
			_ = os.Remove(path)
		}
	}, false
}

// askRunning asks the instance recorded in path to show its window. True
// only if Siphon answered.
func askRunning(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	port := strings.TrimSpace(string(b))
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
	if err != nil {
		return false // stale file: the instance that wrote it is gone
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte(showRequest + "\n")); err != nil {
		return false
	}
	line, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line) == showReply
}
