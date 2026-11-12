//go:build !windows

package extract

import "os/exec"

func hideWindow(*exec.Cmd) {}
