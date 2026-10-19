package browser

import (
	"os/exec"
	"syscall"
)

const (
	createBreakawayFromJob = 0x01000000
	createNewProcessGroup  = 0x00000200
)

// StartDetached starts the program so that it outlives its starter. Browsers
// run their native messaging host inside a job object that ends with it; a
// Siphon window started from the host must not end with it, so it breaks
// away from the job. If the job doesn't allow that, it starts inside it.
func StartDetached(path string, args ...string) error {
	start := func(flags uint32) error {
		cmd := exec.Command(path, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	if err := start(createBreakawayFromJob | createNewProcessGroup); err == nil {
		return nil
	}
	return start(createNewProcessGroup)
}
