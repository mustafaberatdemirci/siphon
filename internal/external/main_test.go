package external

import (
	"os"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

// TestMain lets the test binary play yt-dlp or gallery-dl when a test starts
// it as one (testutil.FakeToolMain); otherwise it runs the tests.
func TestMain(m *testing.M) {
	testutil.FakeToolMain()
	os.Exit(m.Run())
}
