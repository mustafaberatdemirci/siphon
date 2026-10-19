package instance

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond for up to two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal(what)
}

// A second launch finds the running instance, asks it to show its window and
// is told to exit; once the first one is gone, a launch becomes the instance.
func TestSecondLaunchShowsTheRunningInstance(t *testing.T) {
	dir := t.TempDir()
	var shown atomic.Int32
	release, running := Claim(dir, Handlers{Show: func() { shown.Add(1) }})
	if running {
		t.Fatal("the first launch thought another instance was running")
	}
	_, running2 := Claim(dir, Handlers{Show: func() { t.Error("the second launch became an instance too") }})
	if !running2 {
		t.Fatal("the second launch didn't find the running instance")
	}
	waitFor(t, "the running instance was never asked to show", func() bool { return shown.Load() == 1 })

	release()
	if _, err := os.Stat(filepath.Join(dir, fileName)); !os.IsNotExist(err) {
		t.Error("the port file outlived its instance")
	}
	release3, running3 := Claim(dir, Handlers{})
	defer release3()
	if running3 {
		t.Error("after the instance quit, a new launch still thought one was running")
	}
}

// Links handed to the running instance arrive whole, in order.
func TestAddHandsLinksToTheInstance(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var got []string
	release, _ := Claim(dir, Handlers{Add: func(links []string) {
		mu.Lock()
		got = append(got, links...)
		mu.Unlock()
	}})
	defer release()

	links := []string{"https://mega.nz/file/AbCd#key", "https://gofile.io/d/abcd", "yt-dlp:https://video.example/watch?v=1"}
	if err := Add(dir, append(links, "  ", "")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the links never arrived", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == len(links)
	})
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, " ") != strings.Join(links, " ") {
		t.Errorf("got %q", got)
	}
}

// Without the instance's key nothing is added: the key is in a file only the
// user can read, so a program that merely found the port can't queue links.
func TestAddNeedsTheKey(t *testing.T) {
	dir := t.TempDir()
	var added atomic.Int32
	release, _ := Claim(dir, Handlers{Add: func([]string) { added.Add(1) }})
	defer release()
	port, _, err := readFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, first := range []string{addRequest, addRequest + " wrongkey", addRequest + " "} {
		c, err := net.Dial("tcp", "127.0.0.1:"+port)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "%s\nhttps://x.test/a\n\n", first)
		buf := make([]byte, 64)
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _ := c.Read(buf)
		c.Close()
		if strings.Contains(string(buf[:n]), reply) {
			t.Errorf("%q was answered %q", first, buf[:n])
		}
	}
	time.Sleep(100 * time.Millisecond)
	if added.Load() != 0 {
		t.Error("links were added without the key")
	}
}

// With no instance running, Add says so.
func TestAddWithoutAnInstance(t *testing.T) {
	if err := Add(t.TempDir(), []string{"https://x.test/a"}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Add = %v, want ErrNotRunning", err)
	}
}

// A port file left by a crash must not stop the app from starting: nobody
// listening, or some other program on that port, isn't Siphon.
func TestStaleOrForeignPortIsNotAnInstance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileName)

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	_ = os.WriteFile(path, []byte(strconv.Itoa(dead)), 0o644)
	release, running := Claim(dir, Handlers{})
	if running {
		t.Fatal("a stale port file was taken for a running instance")
	}
	release()

	other, _ := net.Listen("tcp", "127.0.0.1:0")
	defer other.Close()
	go func() {
		for {
			c, err := other.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			c.Close()
		}
	}()
	_ = os.WriteFile(path, []byte(strconv.Itoa(other.Addr().(*net.TCPAddr).Port)), 0o644)
	release2, running2 := Claim(dir, Handlers{})
	defer release2()
	if running2 {
		t.Fatal("a foreign program on the port was taken for Siphon")
	}
}

// Without a folder for the port file there is no guard, and no failure either.
func TestNoInstanceGuardWithoutFolder(t *testing.T) {
	release, running := Claim("", Handlers{})
	release()
	if running {
		t.Fatal("running with no folder")
	}
}
