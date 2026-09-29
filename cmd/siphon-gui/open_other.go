//go:build !windows

package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
)

// openInExplorer opens the folder with the system file manager on non-Windows
// systems. Selecting a file isn't supported; the file's folder is opened.
func openInExplorer(path string, selectFile bool) error {
	if selectFile {
		path = filepath.Dir(path)
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}
