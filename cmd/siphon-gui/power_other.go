//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
)

// sleepComputer puts the computer to sleep.
func sleepComputer() error {
	if runtime.GOOS == "darwin" {
		return exec.Command("/usr/bin/pmset", "sleepnow").Run()
	}
	return systemctl("suspend")
}

// shutdownComputer shuts the computer down the usual way: programs are
// asked to close.
func shutdownComputer() error {
	if runtime.GOOS == "darwin" {
		return exec.Command("/usr/bin/osascript", "-e", `tell application "System Events" to shut down`).Run()
	}
	return systemctl("poweroff")
}

// systemctl runs systemd's command (logind lets the user at the seat do it).
func systemctl(verb string) error {
	for _, p := range []string{"/usr/bin/systemctl", "/bin/systemctl"} {
		if _, err := os.Stat(p); err == nil {
			return exec.Command(p, verb).Run()
		}
	}
	return errors.New("systemctl not found: Siphon can't " + verb + " this computer")
}
