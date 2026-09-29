package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/external"
)

// A line of the install dialog: size once known, purpose, and whether the
// tool is already there or can't be installed right now.
func TestToolLabel(t *testing.T) {
	p := external.Package{Tool: "ffmpeg", Purpose: "merges picture and sound"}
	if got := toolLabel(p, "", nil); !strings.Contains(got, "size unknown") || strings.Contains(got, "installed") {
		t.Errorf("before the release was asked: %q", got)
	}
	p.Size = 196 << 20
	if got := toolLabel(p, "", nil); !strings.Contains(got, "196.0 MB") || !strings.Contains(got, "merges picture and sound") {
		t.Errorf("missing tool: %q", got)
	}
	if got := toolLabel(p, `C:\tools\ffmpeg.exe`, nil); !strings.Contains(got, "installed; tick to update") {
		t.Errorf("installed tool: %q", got)
	}
	if got := toolLabel(p, "", errors.New("no checksum\nsecond line")); !strings.Contains(got, "can't be installed now: no checksum") || strings.Contains(got, "second") {
		t.Errorf("unresolvable tool: %q", got)
	}
}
