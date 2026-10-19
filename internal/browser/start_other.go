//go:build !windows

package browser

import (
	"os/exec"
	"syscall"
)

// StartDetached starts the program in a session of its own, so that it
// outlives its starter: the browser ends its native messaging host's process
// group with it.
func StartDetached(path string, args ...string) error {
	cmd := exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
