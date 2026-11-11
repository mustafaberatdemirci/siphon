package testutil

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every test takes its temp folder from TempDir, not from the testing
// package's own: that one fails the test when Windows can't delete the folder
// at once (a virus scanner still holding a file), with every assertion
// passed. The helper existed and 76 calls in 23 files went around it; one of
// them failed a full run on 2026-11-10.
func TestNoTestUsesTheStandardTempDir(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	needle := []byte("." + "TempDir" + "()") // the testing package's own, on a t, b or tb
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "dist" || name == "build") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range bytes.Split(data, []byte("\n")) {
			if bytes.Contains(line, needle) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s:%d: use testutil.TempDir(t)", filepath.ToSlash(rel), i+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// moduleRoot is the folder holding go.mod, found from the package's folder.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

func TestTempDirIsThereAndRemoved(t *testing.T) {
	var dir string
	t.Run("inner", func(t *testing.T) {
		dir = TempDir(t)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Fatalf("no folder: %v", err)
		}
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the folder is still there after the test: %v", err)
	}
}
