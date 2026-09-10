package dl

import (
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

// tempDir, paylaşılan yardımcıya devrediyor. Aynı Windows temizlik sorunu
// run testlerinde de var; iki kopya zamanla ayrışmıştı.
func tempDir(t *testing.T) string {
	t.Helper()
	return testutil.TempDir(t)
}
