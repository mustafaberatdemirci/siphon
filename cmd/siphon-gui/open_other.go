//go:build !windows

package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
)

// openInExplorer, Windows dışı sistemlerde klasörü sistemin dosya
// yöneticisiyle açar. Dosya seçme desteği yok; dosyanın klasörü açılır.
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
