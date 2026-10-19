package main

// Help → Copy diagnostic info: what a bug report needs, on the clipboard.
// The engine's log goes to a file next to the queue file (internal/logfile),
// links' keys and tokens and the user's home folder taken out as it is
// written; the report is its end plus the version and the queue's numbers.
// Nothing is sent anywhere: the user pastes it where they choose.

import (
	"fmt"
	"runtime"
	"sort"
	"strings"

	"fyne.io/fyne/v2"

	"github.com/mustafaberatdemirci/siphon/internal/logfile"
	"github.com/mustafaberatdemirci/siphon/internal/queue"
)

// diagnosticLines is how much of the log's end the report carries.
const diagnosticLines = 200

// appLog is the window's log; nil when there is no folder for it.
var appLog *logfile.File

// stateOrder is the order states are counted in.
var stateOrder = []queue.State{queue.StateRunning, queue.StateQueued, queue.StateWaiting,
	queue.StatePaused, queue.StateStopped, queue.StateFailed, queue.StateDone, queue.StateSkipped}

// diagnosticInfo is the report: plain English, whatever the window's
// language, since it is read by whoever fixes the bug.
func diagnosticInfo(jobs []queue.Job, l *logfile.File) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Siphon %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Fprintf(&b, "Language: %s\n", current)

	perSite := map[string]map[queue.State]int{}
	for _, j := range jobs {
		if perSite[j.Site] == nil {
			perSite[j.Site] = map[queue.State]int{}
		}
		perSite[j.Site][j.State]++
	}
	sites := make([]string, 0, len(perSite))
	for s := range perSite {
		sites = append(sites, s)
	}
	sort.Strings(sites)
	fmt.Fprintf(&b, "Jobs: %d\n", len(jobs))
	for _, s := range sites {
		var parts []string
		for _, st := range stateOrder {
			if n := perSite[s][st]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, st))
			}
		}
		fmt.Fprintf(&b, "  %s: %s\n", s, strings.Join(parts, ", "))
	}

	if l == nil {
		b.WriteString("Log: none\n")
		return b.String()
	}
	tail := l.Tail(diagnosticLines)
	fmt.Fprintf(&b, "Log: %s (last %d lines)\n", logfile.Redact(l.Path()), len(tail))
	for _, line := range tail {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// copyDiagnostics puts the report on the clipboard and says what's in it.
func (q *queueTab) copyDiagnostics() {
	fyne.CurrentApp().Clipboard().SetContent(diagnosticInfo(q.eng.Jobs(), appLog))
	showInfo(T("Copy diagnostic info"),
		T("Copied to the clipboard: Siphon's version, your system, the number of downloads per site and the end of the log. Links' keys and tokens and your user folder are left out. Paste it into a bug report on GitHub."),
		q.win)
}

// logWriter lets the standard log package write to the log file, one entry
// per call, stamped and redacted there.
type logWriter struct{ l *logfile.File }

func (w logWriter) Write(p []byte) (int, error) {
	w.l.Printf("%s", p)
	return len(p), nil
}
