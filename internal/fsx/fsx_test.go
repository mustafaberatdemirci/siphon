package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRenameMovesTheFile(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Rename(a, b); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(b); string(got) != "x" {
		t.Errorf("b holds %q", got)
	}
}

// A missing file is no reason to wait.
func TestRenameGivesUpAtOnceOnOtherErrors(t *testing.T) {
	start := time.Now()
	err := Rename(filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "x"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("waited %v for a file that isn't there", d)
	}
}

// What an antivirus does: it holds the target open for a moment. A plain
// rename fails; Rename waits for it to let go.
func TestRenameWaitsForAnotherProgram(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("an open file stops a rename only on Windows")
	}
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "x.part"), filepath.Join(dir, "x.bin")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(dst) // Go opens without FILE_SHARE_DELETE
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src, dst); err == nil {
		held.Close()
		t.Skip("this system renames over an open file; nothing to wait for")
	} else if !transient(err) {
		held.Close()
		t.Fatalf("the refusal isn't recognized: %v", err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		held.Close()
	}()
	if err := Rename(src, dst); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "new" {
		t.Errorf("dst holds %q", got)
	}
}
