package extract

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW: 7-Zip is a console program, and
// started from the window version (which has no console) it would open one.
const createNoWindow = 0x08000000

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
