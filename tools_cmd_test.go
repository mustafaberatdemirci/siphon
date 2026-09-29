package main

import (
	"strings"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/external"
)

func toolNames(ps []external.Package) string {
	var n []string
	for _, p := range ps {
		n = append(n, p.Tool)
	}
	return strings.Join(n, ",")
}

// "siphon tools install yt-dlp" installs yt-dlp only. MEASURED bug: once the
// named tool was matched the list of names was empty, which read as "no
// names given", and every missing tool was installed too.
func TestChooseTools(t *testing.T) {
	pkgs := external.Packages("windows", "amd64")
	nothing := external.Tools{}
	someFound := external.Tools{YtDlp: `C:\t\yt-dlp.exe`, FFmpeg: `C:\t\ffmpeg.exe`}

	for _, c := range []struct {
		found external.Tools
		names []string
		want  string
	}{
		{nothing, []string{"yt-dlp"}, "yt-dlp"},
		{nothing, []string{"FFmpeg", "deno"}, "ffmpeg,deno"},
		{nothing, nil, "yt-dlp,ffmpeg,gallery-dl,deno"},
		{someFound, nil, "gallery-dl,deno"},       // only the missing ones
		{someFound, []string{"yt-dlp"}, "yt-dlp"}, // named: update even if there
	} {
		got, err := chooseTools(pkgs, c.found, c.names, "windows/amd64")
		if err != nil || toolNames(got) != c.want {
			t.Errorf("names %v: got %q (%v), want %q", c.names, toolNames(got), err, c.want)
		}
	}

	if _, err := chooseTools(pkgs, nothing, []string{"nosuch"}, "windows/amd64"); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("unknown tool: %v", err)
	}
	if _, err := chooseTools(external.Packages("linux", "amd64"), nothing, []string{"ffmpeg"}, "linux/amd64"); err == nil || !strings.Contains(err.Error(), "package manager") {
		t.Errorf("ffmpeg on linux: %v", err)
	}
}
