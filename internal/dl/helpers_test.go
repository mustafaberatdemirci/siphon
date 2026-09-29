package dl

import (
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

// tempDir delegates to the shared helper. The run tests have the same Windows
// cleanup problem; two copies had drifted apart over time.
func tempDir(t *testing.T) string {
	t.Helper()
	return testutil.TempDir(t)
}
