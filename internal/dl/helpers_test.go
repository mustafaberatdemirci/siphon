package dl

import (
	"os"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

// tempDir delegates to the shared helper. The run tests have the same Windows
// cleanup problem; two copies had drifted apart over time.
func tempDir(t *testing.T) string {
	t.Helper()
	return testutil.TempDir(t)
}

// ownEntries lists dir without files another program left there
// (testutil.OwnEntries says why).
func ownEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	return testutil.OwnEntries(t, dir)
}
