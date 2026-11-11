package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/logfile"
	"github.com/mustafaberatdemirci/siphon/internal/queue"
	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

// The report carries the version, the queue's numbers per site and the log's
// end, with no link's key in it.
func TestDiagnosticInfo(t *testing.T) {
	l, err := logfile.Open(filepath.Join(testutil.TempDir(t), "siphon.log"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < diagnosticLines+20; i++ {
		l.Printf("line %d", i)
	}
	l.Printf("ERROR https://mega.nz/file/AbCd#s3cretKey: HTTP 509")

	jobs := []queue.Job{
		{Site: "bunkr", State: queue.StateFailed},
		{Site: "bunkr", State: queue.StateRunning},
		{Site: "bunkr", State: queue.StateFailed},
		{Site: "mega", State: queue.StateDone},
	}
	got := diagnosticInfo(jobs, l)
	for _, want := range []string{
		"Siphon " + version + " (",
		"Jobs: 4\n",
		"  bunkr: 1 running, 2 failed\n",
		"  mega: 1 done\n",
		"(last 200 lines)",
		"https://mega.nz/file/AbCd#…: HTTP 509",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "s3cretKey") {
		t.Error("the report carries a link's key")
	}
	if strings.Contains(got, "line 0\n") {
		t.Error("the report carries more than the log's end")
	}

	if got := diagnosticInfo(nil, nil); !strings.Contains(got, "Log: none") {
		t.Errorf("without a log: %s", got)
	}
}

// failingWriter is the window version's stderr: there is none.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, os.ErrInvalid }

// The window version has no stderr, and with stderr first in the chain not
// one error line reached the log file.
func TestLogReachesTheFileWithoutStderr(t *testing.T) {
	l, err := logfile.Open(filepath.Join(testutil.TempDir(t), "siphon.log"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	lg := log.New(logOutput(l, failingWriter{}), "", 0)
	lg.Printf("ERROR   %s: %v", "a.bin", "retry budget exhausted")
	lg.Printf("ERROR second line")
	tail := l.Tail(10)
	if len(tail) != 2 || !strings.Contains(tail[0], "ERROR   a.bin: retry budget exhausted") {
		t.Fatalf("the log file holds %q", tail)
	}
}
