//go:build !windows

package external

import "os/exec"

func hideWindow(*exec.Cmd) {}
