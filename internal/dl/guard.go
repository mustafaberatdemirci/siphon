package dl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mustafaberatdemirci/siphon/internal/external"
)

// A download must never create a file that Siphon itself would later read as
// its configuration or start as one of its external tools: the file's name
// and content come from the site. Such names only matter in the folders they
// are looked up in: sites.toml next to Siphon's executable, the tools in
// Siphon's tools folder and in the PATH folders. Anywhere else they are
// ordinary files and download as usual.

// ErrProtectedName is returned when a download would create such a file. It
// is not retryable: the same link always gives the same name.
var ErrProtectedName = errors.New("refusing to save a file Siphon would read or run")

// configName is the site definitions file read from next to the executable.
const configName = "sites.toml"

// toolNames are the external tools Siphon looks up (external.Find), plus
// ffprobe, which "Install tools" puts next to ffmpeg.
var toolNames = map[string]bool{"yt-dlp": true, "gallery-dl": true, "ffmpeg": true, "ffprobe": true, "deno": true}

// protectedDirs returns the executable's folder (where sites.toml is read)
// and the folders tools are looked up in. A variable so tests can point it
// elsewhere.
var protectedDirs = func() (exeDir string, toolDirs []string) {
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
		toolDirs = append(toolDirs, exeDir)
	}
	if d, err := external.ToolsDir(); err == nil {
		toolDirs = append(toolDirs, d)
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		// A relative entry is never searched: Go's LookPath refuses what
		// it would find there.
		if filepath.IsAbs(d) {
			toolDirs = append(toolDirs, d)
		}
	}
	return exeDir, toolDirs
}

// checkProtected returns ErrProtectedName if saving to final would put a
// file Siphon reads or runs where it looks for it.
func checkProtected(final string) error {
	name := strings.ToLower(filepath.Base(final))
	ext := filepath.Ext(name)
	isConfig := name == configName
	isTool := toolNames[strings.TrimSuffix(name, ext)] && runnableExt(ext)
	if !isConfig && !isTool {
		return nil
	}
	dir := comparablePath(filepath.Dir(final))
	exeDir, toolDirs := protectedDirs()
	refused := isConfig && exeDir != "" && dir == comparablePath(exeDir)
	for _, d := range toolDirs {
		if isTool && d != "" && dir == comparablePath(d) {
			refused = true
		}
	}
	if !refused {
		return nil
	}
	return fmt.Errorf("%w: %s in %s, a folder Siphon reads its settings or tools from; choose another download folder",
		ErrProtectedName, filepath.Base(final), filepath.Dir(final))
}

// comparablePath makes two spellings of one folder equal: absolute, clean
// and case-folded. Windows and macOS ignore case in file names; on Linux
// folding can only make the check stricter.
func comparablePath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return strings.ToLower(filepath.Clean(p))
}

// runnableExt reports whether a tool's name with this extension would be
// started: no extension, or one Windows starts by itself (PATHEXT).
func runnableExt(ext string) bool {
	switch ext {
	case "", ".exe", ".com", ".bat", ".cmd":
		return true
	}
	if runtime.GOOS == "windows" {
		for _, e := range filepath.SplitList(os.Getenv("PATHEXT")) {
			if strings.EqualFold(e, ext) {
				return true
			}
		}
	}
	return false
}
