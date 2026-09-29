package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// A second launch finds the running instance, asks it to show its window and
// is told to exit; once the first one is gone, a launch becomes the instance.
func TestSecondLaunchShowsTheRunningInstance(t *testing.T) {
	dir := t.TempDir()
	var shown atomic.Int32
	release, running := claimInstance(dir, func() { shown.Add(1) })
	if running {
		t.Fatal("the first launch thought another instance was running")
	}

	_, running2 := claimInstance(dir, func() { t.Error("the second launch became an instance too") })
	if !running2 {
		t.Fatal("the second launch didn't find the running instance")
	}
	deadline := time.Now().Add(2 * time.Second)
	for shown.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if shown.Load() != 1 {
		t.Fatalf("the running instance was asked to show %d times, want 1", shown.Load())
	}

	release()
	if _, err := os.Stat(filepath.Join(dir, instanceFile)); !os.IsNotExist(err) {
		t.Error("the port file outlived its instance")
	}
	release3, running3 := claimInstance(dir, func() {})
	defer release3()
	if running3 {
		t.Error("after the instance quit, a new launch still thought one was running")
	}
}

// A port file left by a crash must not stop the app from starting: nobody
// listening, or some other program on that port, isn't Siphon.
func TestStaleOrForeignPortIsNotAnInstance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, instanceFile)

	// Nobody listening on the recorded port.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	_ = os.WriteFile(path, []byte(strconv.Itoa(dead)), 0o644)
	release, running := claimInstance(dir, func() {})
	if running {
		t.Fatal("a stale port file was taken for a running instance")
	}
	release()

	// Another program on the recorded port, answering something else.
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
	release2, running2 := claimInstance(dir, func() {})
	defer release2()
	if running2 {
		t.Fatal("a foreign program on the port was taken for Siphon")
	}
}

// Without a folder for the port file there is no guard, and no failure either.
func TestNoInstanceGuardWithoutFolder(t *testing.T) {
	release, running := claimInstance("", func() {})
	release()
	if running {
		t.Fatal("running with no folder")
	}
}
