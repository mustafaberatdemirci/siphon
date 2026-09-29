package dl

import (
	"os"
	"strings"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

// tempDir delegates to the shared helper. The run tests have the same Windows
// cleanup problem; two copies had drifted apart over time.
func tempDir(t *testing.T) string {
	t.Helper()
	return testutil.TempDir(t)
}

// ownEntries lists dir without files another program left there. A virus
// scanner on the development machine (Kaspersky was active when this was
// measured) sometimes leaves "<NAME>.PART.STATE.tmp" copies of the state
// files it scans; Siphon never writes a name ending in ".tmp" (its own
// temp file is ".part.stmp"), so dropping them can't hide a leftover of its
// own. Counting them failed the file-count tests about once in 16 runs.
func ownEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	own := entries[:0]
	for _, e := range entries {
		if strings.HasSuffix(strings.ToLower(e.Name()), ".tmp") {
			t.Logf("ignoring %s, written by another program", e.Name())
			continue
		}
		own = append(own, e)
	}
	return own
}
