package extract

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// un7z unpacks with 7-Zip into root. -p- gives an empty password instead
// of asking for one: a protected archive fails at once instead of waiting
// for an answer no one gives.
//
// 7-Zip has no limit of its own on what it writes, so what the archive says
// it holds is added up first (listed7z): past limit, nothing is unpacked.
// The bomb limit only covered the zip and tar archives Go reads before.
func un7z(ctx context.Context, sevenZip, first, root string, limit int64) error {
	if sevenZip == "" {
		return ErrNeeds7Zip
	}
	size, err := listed7z(ctx, sevenZip, first)
	if err != nil {
		return err
	}
	if size > limit {
		return ErrTooLarge
	}
	cmd := exec.CommandContext(ctx, sevenZip, "x", "-y", "-p-", "-bsp0", "-o"+root, "--", first)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 5 * time.Second
	hideWindow(cmd)
	err = cmd.Run()
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

// listedTail is how many of the listing's last lines are kept for an error.
const listedTail = 20

// listed7z adds up the sizes 7-Zip lists for an archive's entries: what
// unpacking it would write. With -slt every entry has a "Size = N" line;
// the lines before the "----------" one are about the archive itself
// ("Physical Size" among them). The listing is read as it comes, not kept:
// an archive of millions of tiny files lists hundreds of megabytes.
func listed7z(ctx context.Context, sevenZip, first string) (int64, error) {
	cmd := exec.CommandContext(ctx, sevenZip, "l", "-slt", "-p-", "--", first)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	var (
		total   int64
		entries bool
		tail    []string
	)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if tail = append(tail, line); len(tail) > listedTail {
			tail = tail[1:]
		}
		if line == "----------" {
			entries = true
			continue
		}
		if v, ok := strings.CutPrefix(line, "Size = "); ok && entries {
			if n, perr := strconv.ParseInt(strings.TrimSpace(v), 10, 64); perr == nil && n > 0 {
				total += n
			}
		}
	}
	_, _ = io.Copy(io.Discard, stdout) // a line too long stops the scanner; 7-Zip must still finish
	err = cmd.Wait()
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil {
		text := strings.Join(tail, "\n") + "\n" + stderr.String()
		if strings.Contains(strings.ToLower(text), "password") {
			return 0, ErrEncrypted
		}
		return 0, fmt.Errorf("7-Zip: %s", lastError(text, err))
	}
	return total, nil
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
