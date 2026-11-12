package extract

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// un7z unpacks with 7-Zip into root. -p- gives an empty password instead
// of asking for one: a protected archive fails at once instead of waiting
// for an answer no one gives.
func un7z(ctx context.Context, sevenZip, first, root string) error {
	if sevenZip == "" {
		return ErrNeeds7Zip
	}
	cmd := exec.CommandContext(ctx, sevenZip, "x", "-y", "-p-", "-bsp0", "-o"+root, "--", first)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 5 * time.Second
	hideWindow(cmd)
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	text := out.String()
	if strings.Contains(strings.ToLower(text), "password") {
		return ErrEncrypted
	}
	return fmt.Errorf("7-Zip: %s", lastError(text, err))
}

// lastError is 7-Zip's last "ERROR" line, or its last line, or err.
func lastError(text string, err error) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if strings.Contains(l, "ERROR") {
			return l
		}
		if last == "" {
			last = l
		}
	}
	if last != "" {
		return last
	}
	return err.Error()
}
