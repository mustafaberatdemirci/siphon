//go:build !windows

package external

import (
	"os/exec"
	"syscall"
)

func hideWindow(*exec.Cmd) {}

// startTree starts cmd as the leader of its own process group, so stopping
// it stops the programs it started too (yt-dlp starts ffmpeg): killing only
// the started process left those running and writing after a pause.
func startTree(cmd *exec.Cmd) (release func(), err error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return func() {}, cmd.Start()
}
