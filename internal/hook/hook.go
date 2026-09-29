// Package hook runs a command line defined by the user.
//
// First use: a script that switches VPN when the MEGA quota runs out
// (MegaBasterd's "run command on 509" feature). The command runs on the
// user's own machine, with the user's own permissions, and is a line the user
// wrote; all that happens here is handing it to the operating system's shell,
// collecting its output and bounding its run time.
package hook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout is a reasonable upper bound for a command that switches VPN.
// If it is exceeded the command is killed; a hanging script must not leave
// the queue stuck at "command running" forever.
const DefaultTimeout = 2 * time.Minute

// outputTail is how much output is shown to the user.
const outputTail = 400

// Run runs the line in a shell (Windows: cmd /S /C, others: sh -c) and
// returns the tail of the combined output. A non-zero exit code is an error.
// If timeout <= 0, DefaultTimeout is used.
func Run(ctx context.Context, line string, timeout time.Duration) (string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", errors.New("empty command")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := shellCommand(ctx, line)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// On timeout the shell is killed, but a child process started by the
	// shell (e.g. a VPN client) can keep the output pipe open; without
	// WaitDelay, Run would hang until that pipe closed.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	tail := tailOf(out.String())
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return tail, fmt.Errorf("command did not finish within %s and was killed", timeout)
		}
		return tail, err
	}
	return tail, nil
}

func tailOf(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= outputTail {
		return s
	}
	return "…" + string(r[len(r)-outputTail:])
}

// exitError lets tests check for "failed with output" independent of the shell.
func exitError(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}
