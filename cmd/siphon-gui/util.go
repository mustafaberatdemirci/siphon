package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// normalizeDir converts a path coming from the user or the folder picker into
// the form Windows expects.
//
// MEASURED: Fyne's folder picker derives the path from a URI and returns it
// with FORWARD SLASHES — picking "E:\x" writes "E:/x" into the box. Go's file
// calls accept forward slashes, so the download lands in the right place and
// the bug stays invisible; but explorer.exe doesn't accept them. When it
// doesn't recognize the path it doesn't even fail, it silently opens the
// Documents folder. That was the behavior the user saw.
//
// Clean also drops a trailing separator, which closes a second trap: on the
// command line explorer gets "E:\x\" quoted, the trailing backslash escapes
// the closing quote and explorer again doesn't recognize the path.
//
// Quotes are stripped too: Windows' "Copy as path" command gives the path in
// quotes and not everyone who pastes it notices.
func normalizeDir(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"`)
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// A bare drive letter ("E:") is a path RELATIVE TO THE DRIVE, not its
	// root: Clean makes it "E:.", i.e. "the current directory of drive E:".
	// That is a place depending on process state; nobody writing "E:" in the
	// folder box means that. We interpret it as the root.
	//
	// If VolumeName equals the WHOLE input we only have the drive letter
	// ("E:"). "E:\", "E:/x" and UNC paths aren't equal and are left alone.
	if s == filepath.VolumeName(s) {
		s += string(filepath.Separator)
	}
	return filepath.Clean(filepath.FromSlash(s))
}

// defaultOutDir picks a reasonable starting folder.
func defaultOutDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		d := filepath.Join(home, "Downloads")
		if fi, serr := os.Stat(d); serr == nil && fi.IsDir() {
			return filepath.Join(d, "siphon")
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// pickOpenTarget picks the most meaningful target for "Open folder".
//
// Order: the last downloaded file (SELECTED in its folder) -> the folder of
// the album that started -> the output root. When the output root is "E:\"
// the files land under "E:\Album\", so opening the root looked to the user
// like "the wrong folder opened"; going to the file itself, like IDM does, is
// the right behavior.
//
// The folder is NOT CREATED SILENTLY: if it is missing, the reason is given.
func pickOpenTarget(lastPath, lastDir, outDir string) (target string, selectFile bool, reason string) {
	if lastPath != "" {
		if fi, err := os.Stat(lastPath); err == nil && !fi.IsDir() {
			return lastPath, true, ""
		}
	}
	if lastDir != "" {
		if fi, err := os.Stat(lastDir); err == nil && fi.IsDir() {
			return lastDir, false, ""
		}
	}
	if outDir == "" {
		return "", false, ""
	}
	fi, err := os.Stat(outDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false, Tf("The folder doesn't exist yet: %s — it will be created when a download starts.", outDir)
	case err != nil:
		return "", false, Tf("Could not access the folder: %s", err.Error())
	case !fi.IsDir():
		return "", false, Tf("This is not a folder: %s", outDir)
	}
	// Given a relative path, explorer would resolve it against ITS OWN working directory.
	if abs, aerr := filepath.Abs(outDir); aerr == nil {
		outDir = abs
	}
	return outDir, false, ""
}

// parseLinks turns the text field into a URL list.
// The SAME rules as the command line's -i file: blank lines and # are skipped.
func parseLinks(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func shortName(s string) string {
	s = filepath.Base(s)
	const max = 60
	if len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-3]) + "..."
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func humanBytes(n int64) string {
	if n < 0 {
		return "? B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	for _, u := range units {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f PB", v/unit)
}

// parseSpeedLimit converts MB/s input like "2", "2.5", "0" into bytes per
// second. Empty or 0 means unlimited; input that can't be understood returns
// an error instead of silently counting as 0.
func parseSpeedLimit(s string) (int64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if s == "" {
		return 0, nil
	}
	var mbps float64
	if _, err := fmt.Sscanf(s, "%g", &mbps); err != nil {
		return 0, fmt.Errorf("%s", Tf("Could not understand the speed limit: %q", s))
	}
	if mbps < 0 {
		return 0, fmt.Errorf("%s", T("The speed limit can't be negative."))
	}
	return int64(mbps * 1024 * 1024), nil
}
