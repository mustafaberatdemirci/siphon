package external

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW: the tools are console programs, and
// started from the GUI (which has no console) each would open a window.
const createNoWindow = 0x08000000

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
