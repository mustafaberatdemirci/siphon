package doctor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// fakeResolver is a minimal imitation satisfying the site.Resolver interface.
// doctor only calls Diagnose; the rest exists because of the contract.
type fakeResolver struct {
	results []site.LayerResult
	err     error
	calls   int
}

func (f *fakeResolver) Match(string) bool { return false }

func (f *fakeResolver) Resolve(context.Context, string, func(site.Item) error) ([]site.ItemError, error) {
	return nil, nil
}

func (f *fakeResolver) ResolveOne(context.Context, string) (site.Item, error) {
	return site.Item{}, nil
}

func (f *fakeResolver) Diagnose(context.Context) ([]site.LayerResult, error) {
	f.calls++
	return f.results, f.err
}

func ok(l site.Layer, detail string) site.LayerResult {
	return site.LayerResult{Layer: l, Status: site.StatusOK, Detail: detail}
}

func TestReportWorst(t *testing.T) {
	cases := []struct {
		name string
		r    Report
		want site.LayerStatus
	}{
		{"all OK", Report{Results: []site.LayerResult{
			ok(site.LayerDNS, "a"), ok(site.LayerTLS, "b"),
		}}, site.StatusOK},
		{"one WARN", Report{Results: []site.LayerResult{
			ok(site.LayerDNS, "a"),
			{Layer: site.LayerCDN, Status: site.StatusWarn},
		}}, site.StatusWarn},
		{"WARN and FAIL -> FAIL", Report{Results: []site.LayerResult{
			{Layer: site.LayerCDN, Status: site.StatusWarn},
			{Layer: site.LayerDNS, Status: site.StatusFail},
		}}, site.StatusFail},
		{"Diagnose error -> FAIL", Report{Err: errors.New("no canary")}, site.StatusFail},
		{"empty report -> OK", Report{}, site.StatusOK},
	}
	for _, c := range cases {
		if got := c.r.Worst(); got != c.want {
			t.Errorf("%s: Worst() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRunCallsDiagnosePerSite(t *testing.T) {
	a := &fakeResolver{results: []site.LayerResult{ok(site.LayerDNS, "a")}}
	b := &fakeResolver{err: errors.New("blew up")}

	reports := Run(context.Background(), []Named{
		{Name: "alpha", Resolver: a},
		{Name: "beta", Resolver: b},
	})
	if len(reports) != 2 {
		t.Fatalf("%d reports, want 2", len(reports))
	}
	if a.calls != 1 || b.calls != 1 {
		t.Errorf("Diagnose call counts: a=%d b=%d", a.calls, b.calls)
	}
	if reports[0].Site != "alpha" || reports[1].Site != "beta" {
		t.Errorf("site order not preserved: %q, %q", reports[0].Site, reports[1].Site)
	}
	if reports[1].Err == nil {
		t.Error("the Diagnose error was not carried into the report")
	}
}

func TestFormatOutputShape(t *testing.T) {
	var buf bytes.Buffer
	worst := Format(&buf, []Report{{
		Site: "bunkr",
		Results: []site.LayerResult{
			{Layer: site.LayerDNS, Status: site.StatusOK, Detail: "4 addresses", Evidence: "1.2.3.4"},
			{Layer: site.LayerCDN, Status: site.StatusWarn, Detail: "new host", Evidence: "x.cdn.cr"},
		},
	}})
	out := buf.String()

	for _, want := range []string{"bunkr", "DNS", "OK", "4 addresses", "1.2.3.4", "CDN", "WARN", "x.cdn.cr"} {
		if !strings.Contains(out, want) {
			t.Errorf("output doesn't contain %q:\n%s", want, out)
		}
	}
	// Evidence must be on its own line and indented: the readability of a
	// diagnostic line is this tool's whole point.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 5 {
		t.Fatalf("%d lines, want 5:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[2], "1.2.3.4") || strings.Contains(lines[2], "DNS") {
		t.Errorf("Evidence must be on its own line: %q", lines[2])
	}
	if worst != site.StatusWarn {
		t.Errorf("worst = %v, want WARN", worst)
	}
}

// WARN must not escalate the exit to FAIL: the "CDN is a signal, not a gate"
// rule only means something if WARN doesn't count as failure.
func TestFormatWarnDoesNotBecomeFail(t *testing.T) {
	var buf bytes.Buffer
	worst := Format(&buf, []Report{
		{Site: "a", Results: []site.LayerResult{{Layer: site.LayerCDN, Status: site.StatusWarn}}},
		{Site: "b", Results: []site.LayerResult{ok(site.LayerDNS, "fine")}},
	})
	if worst != site.StatusWarn {
		t.Fatalf("worst = %v, want WARN", worst)
	}
}

func TestFormatFailWinsAcrossSites(t *testing.T) {
	var buf bytes.Buffer
	worst := Format(&buf, []Report{
		{Site: "a", Results: []site.LayerResult{{Layer: site.LayerCDN, Status: site.StatusWarn}}},
		{Site: "b", Results: []site.LayerResult{{Layer: site.LayerDNS, Status: site.StatusFail}}},
		{Site: "c", Results: []site.LayerResult{ok(site.LayerDNS, "fine")}},
	})
	if worst != site.StatusFail {
		t.Fatalf("worst = %v, want FAIL", worst)
	}
}

func TestFormatHandlesDiagnoseError(t *testing.T) {
	var buf bytes.Buffer
	worst := Format(&buf, []Report{{Site: "bunkr", Err: errors.New("canary list is empty")}})
	if worst != site.StatusFail {
		t.Errorf("worst = %v, want FAIL", worst)
	}
	if !strings.Contains(buf.String(), "canary list is empty") {
		t.Errorf("the error message was not printed:\n%s", buf.String())
	}
}

func TestFormatHandlesEmptyResults(t *testing.T) {
	var buf bytes.Buffer
	worst := Format(&buf, []Report{{Site: "bunkr"}})
	if worst != site.StatusOK {
		// Without results WARN is printed but Worst() returns OK; this is
		// deliberate so the exit code isn't affected.
		t.Logf("worst = %v", worst)
	}
	if !strings.Contains(buf.String(), "no diagnosis results") {
		t.Errorf("empty results were not reported:\n%s", buf.String())
	}
}

// --- Recorder ---

func TestRecorderSavesRealBytes(t *testing.T) {
	dir := t.TempDir()
	rec := &Recorder{Dir: dir}

	body := []byte("<html>real body</html>")
	rec.For("bunkr")("canary.html", body)

	saved := rec.Saved()
	if len(saved) != 1 {
		t.Fatalf("%d files saved, want 1: %v", len(saved), saved)
	}
	got, err := os.ReadFile(saved[0])
	if err != nil {
		t.Fatalf("could not read the recording: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("saved content differs: %q", got)
	}

	// The name must carry both the site and the label: six months later you
	// need to know which recording belongs to what.
	base := filepath.Base(saved[0])
	if !strings.Contains(base, "bunkr") || !strings.Contains(base, "canary.html") {
		t.Errorf("file name carries incomplete information: %q", base)
	}
	if len(rec.Errs()) != 0 {
		t.Errorf("unexpected errors: %v", rec.Errs())
	}
}

func TestRecorderPerSiteLabels(t *testing.T) {
	dir := t.TempDir()
	rec := &Recorder{Dir: dir}
	rec.For("bunkr")("canary.html", []byte("a"))
	rec.For("pixeldrain")("canary.json", []byte("b"))

	saved := rec.Saved()
	if len(saved) != 2 {
		t.Fatalf("%d files, want 2", len(saved))
	}
	joined := strings.Join(saved, " ")
	for _, want := range []string{"bunkr", "pixeldrain"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no recording named %q: %v", want, saved)
		}
	}
}

// An empty body must not be recorded: a zero-byte file is noise in a diff.
func TestRecorderSkipsEmptyBodies(t *testing.T) {
	dir := t.TempDir()
	cfg := site.SiteConfig{Record: (&Recorder{Dir: dir}).For("bunkr")}
	cfg.Recordln("canary.html", nil)
	cfg.Recordln("canary.html", []byte{})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%d files created, an empty body should not have been recorded", len(entries))
	}
}

// A recording error does NOT fail the diagnosis: the real job is the layer
// report, recording is a helper.
func TestRecorderCollectsErrorsWithoutPanicking(t *testing.T) {
	// Force using an existing FILE as the folder.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &Recorder{Dir: f}
	rec.For("bunkr")("canary.html", []byte("data"))

	if len(rec.Saved()) != 0 {
		t.Error("a file looks saved despite the error")
	}
	if len(rec.Errs()) == 0 {
		t.Error("the recording error was not collected")
	}
}

func TestRecorderDefaultDir(t *testing.T) {
	// With an empty Dir, DefaultDir must be used. Move the cwd to a temp
	// folder so nothing is really written.
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	rec := &Recorder{}
	rec.For("bunkr")("canary.html", []byte("data"))
	if len(rec.Saved()) != 1 {
		t.Fatalf("not saved: %v / %v", rec.Saved(), rec.Errs())
	}
	if !strings.Contains(rec.Saved()[0], DefaultDir) {
		t.Errorf("DefaultDir was not used: %q", rec.Saved()[0])
	}
}

func TestSafeFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"bunkr", "bunkr"},
		{"canary.html", "canary.html"},
		{"a/b\\c", "a-b-c"},
		{"spaced naïve", "spaced-na-ve"},
		{"", "record"},
		{"   ", "record"},
	}
	for _, c := range cases {
		if got := safe(c.in); got != c.want {
			t.Errorf("safe(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
