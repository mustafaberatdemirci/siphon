//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// explorerPath returns explorer.exe with its FULL PATH: a bare name is
// resolved via %PATH%, and an explorer.exe dropped into a writable PATH
// directory would run from this button.
func explorerPath() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		return filepath.Join(root, "explorer.exe")
	}
	return "explorer"
}

// openInExplorer opens a folder, or opens a file SELECTED in its folder.
//
// MEASURED (2026-09-11): Go's exec package quotes an argument containing
// spaces as a whole and the command line becomes
// `explorer "/select,E:\My Album\x.mp4"`. explorer doesn't recognize that and
// silently opens Documents. The only working form quotes just the path:
// `explorer /select,"E:\My Album\x.mp4"`. Go can't produce that form, so the
// command line is built by hand here.
func openInExplorer(path string, selectFile bool) error {
	exe := explorerPath()
	cmd := exec.Command(exe)
	if selectFile {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			CmdLine: fmt.Sprintf(`"%s" /select,"%s"`, exe, path),
		}
	} else {
		cmd.Args = []string{exe, path}
	}
	// explorer.exe returns 1 EVEN ON SUCCESS; the exit code isn't checked,
	// only a start failure is meaningful.
	return cmd.Start()
}
