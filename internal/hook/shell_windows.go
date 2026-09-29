package hook

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCommand hands the line to cmd.exe.
//
// Go's argument escaping is wrong for cmd: passing "/C" and the line as
// separate arguments quotes the line if it contains spaces, and cmd takes it
// as a single program name. So the command line is written raw. With /S, cmd
// strips the first and last quote and processes what's between as is; that
// way the user's line survives even with paths and arguments containing
// spaces: cmd /S /C "  "C:\vpn\switch.bat" us  ".
//
// HideWindow: the GUI is built without a console (windowsgui), so a black
// window must not pop up every time the quota runs out.
func shellCommand(ctx context.Context, line string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:    `cmd.exe /S /C "` + line + `"`,
		HideWindow: true,
	}
	return cmd
}
