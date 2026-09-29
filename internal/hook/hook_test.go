package hook

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// "echo" exists in both cmd and sh; the output must be collected.
func TestRunCapturesOutput(t *testing.T) {
	out, err := Run(context.Background(), "echo hello quota", 0)
	if err != nil {
		t.Fatalf("Run: %v (%q)", err, out)
	}
	if !strings.Contains(out, "hello quota") {
		t.Errorf("output = %q", out)
	}
}

// A path with spaces + arguments: checks that /S /C quoting on Windows and -c
// on sh don't mangle the line. The command writes to a file in a directory
// whose name contains a space.
func TestRunKeepsQuotedPathsIntact(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dir with spaces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "marker.txt")
	if _, err := Run(context.Background(), `echo ran> "`+marker+`"`, 0); err != nil {
		t.Fatalf("Run: %v", err)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the command did not write the file: %v", err)
	}
	if !strings.Contains(string(b), "ran") {
		t.Errorf("content = %q", b)
	}
}

// A non-zero exit code must be an error; the output must still be returned.
func TestRunReportsFailure(t *testing.T) {
	out, err := Run(context.Background(), "echo something broke&& exit 3", 0)
	if err == nil {
		t.Fatal("a failing command returned no error")
	}
	if !exitError(err) {
		t.Errorf("error type %T: %v", err, err)
	}
	if !strings.Contains(out, "something broke") {
		t.Errorf("the failing command's output was lost: %q", out)
	}
}

// A hanging command must be killed on timeout.
func TestRunKillsOnTimeout(t *testing.T) {
	sleep := "sleep 5"
	if isWindows() {
		sleep = "ping -n 6 127.0.0.1 >nul"
	}
	start := time.Now()
	_, err := Run(context.Background(), sleep, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("expected a timeout error: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("the command was not killed, waited %s", time.Since(start))
	}
}

func TestRunRejectsEmpty(t *testing.T) {
	if _, err := Run(context.Background(), "   ", 0); err == nil {
		t.Fatal("an empty command was accepted")
	}
}
