// Package doctor runs the layer diagnosis and prints a readable report.
//
// This package is the tool's reason to exist. The real cost of writing a
// scraper isn't the time to write the code, it is seeing "0 files
// downloaded" six months later and not knowing why. doctor removes that
// guesswork: it tells within seconds which layer broke and saves the broken
// response to disk.
package doctor

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/netcheck"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// Named pairs a resolver with its name. site.Resolver doesn't carry its name.
type Named struct {
	Name     string
	Resolver site.Resolver
	// Host is the site's host to check the network against when no failing
	// layer names one (see HostOf); "" skips that check.
	Host string
}

// HostOf is the host a site is diagnosed on: its first canary's, or its
// first domain without a wildcard.
func HostOf(cfg site.SiteConfig) string {
	for _, c := range cfg.CanaryURLs {
		if u, err := url.Parse(c); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	for _, d := range cfg.Domains {
		if !strings.Contains(d, "*") {
			return d
		}
	}
	return ""
}

// LayerNetwork is doctor's own line: whether the user's network blocks the
// site (internal/netcheck). It comes after a DNS, TLS or Fetch failure,
// which on its own says "timeout" and sends the user looking for the fault
// in Siphon.
const LayerNetwork site.Layer = "Network"

// checkNetwork is netcheck.Check; tests play a network instead.
var checkNetwork = netcheck.Check

// Report is the diagnosis result of a single site.
type Report struct {
	Site    string
	Results []site.LayerResult
	// Err is set when Diagnose itself couldn't run (e.g. an empty canary list).
	Err error
}

// Worst returns the worst status in the report.
func (r Report) Worst() site.LayerStatus {
	if r.Err != nil {
		return site.StatusFail
	}
	worst := site.StatusOK
	for _, res := range r.Results {
		switch res.Status {
		case site.StatusFail:
			return site.StatusFail
		case site.StatusWarn:
			worst = site.StatusWarn
		}
	}
	return worst
}

// Run runs the diagnosis for each site.
//
// Sites run SEQUENTIALLY, not in parallel: doctor's output is for humans and
// interleaved lines make diagnosis harder. Also, hitting a rate limit during
// diagnosis would break the very thing being diagnosed.
func Run(ctx context.Context, sites []Named) []Report {
	out := make([]Report, 0, len(sites))
	for _, s := range sites {
		res, err := s.Resolver.Diagnose(ctx)
		if host, ok := networkFailure(res, s.Host); ok {
			res = withNetwork(res, checkNetwork(ctx, host))
		}
		out = append(out, Report{Site: s.Name, Results: res, Err: err})
	}
	return out
}

// networkFailure reports whether a layer the network can break failed, and
// the host it failed on: the one its evidence names, or fallback.
func networkFailure(res []site.LayerResult, fallback string) (string, bool) {
	for _, r := range res {
		if r.Status != site.StatusFail || (r.Layer != site.LayerDNS && r.Layer != site.LayerTLS && r.Layer != site.LayerFetch) {
			continue
		}
		host := hostIn(r.Evidence)
		if host == "" {
			host = fallback
		}
		return host, host != ""
	}
	return "", false
}

var (
	urlIn  = regexp.MustCompile(`https?://[^\s"'()]+`)
	hostAt = regexp.MustCompile(`^([a-z0-9-]+(\.[a-z0-9-]+)+): `) // DNS evidence: "host: error"
)

// hostIn finds the host a failure's evidence is about.
func hostIn(evidence string) string {
	if m := urlIn.FindString(evidence); m != "" {
		if u, err := url.Parse(m); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	if m := hostAt.FindStringSubmatch(strings.ToLower(evidence)); m != nil {
		return m[1]
	}
	return ""
}

// withNetwork adds the network's line, and corrects the lines a site's
// diagnosis wrote as OK only because its failure came later: the DNS line
// when the address is a block page, the TLS line when the handshake is what
// gets no answer, and the Challenge line then (it couldn't be reached).
func withNetwork(res []site.LayerResult, f netcheck.Finding) []site.LayerResult {
	status := site.StatusWarn
	switch {
	case f.Blocked():
		status = site.StatusFail
	case f.Reached:
		status = site.StatusOK
	}
	out := make([]site.LayerResult, 0, len(res)+1)
	for _, r := range res {
		if r.Status == site.StatusOK {
			switch {
			case f.DNSBlocked && r.Layer == site.LayerDNS:
				r.Status, r.Detail = site.StatusFail, "the address isn't the site's (see Network)"
			case f.NameFiltered && r.Layer == site.LayerTLS:
				r = site.LayerResult{Layer: site.LayerTLS, Status: site.StatusFail,
					Detail: "a handshake naming the site gets no answer (see Network)"}
			case f.NameFiltered && r.Layer == site.LayerChallenge:
				continue
			}
		}
		out = append(out, r)
	}
	return append(out, site.LayerResult{Layer: LayerNetwork, Status: status, Detail: f.Summary(), Evidence: f.Evidence()})
}

// Format writes the reports and returns the worst status.
func Format(w io.Writer, reports []Report) site.LayerStatus {
	worst := site.StatusOK
	for i, r := range reports {
		if i > 0 {
			fmt.Fprintln(w)
		}
		if s := r.Worst(); s == site.StatusFail {
			worst = site.StatusFail
		} else if s == site.StatusWarn && worst != site.StatusFail {
			worst = site.StatusWarn
		}

		fmt.Fprintf(w, "%s\n", r.Site)
		if r.Err != nil {
			fmt.Fprintf(w, "  %-10s %-5s %s\n", "-", site.StatusFail, r.Err)
			continue
		}
		if len(r.Results) == 0 {
			fmt.Fprintf(w, "  %-10s %-5s %s\n", "-", site.StatusWarn, "no diagnosis results")
			continue
		}
		for _, res := range r.Results {
			fmt.Fprintf(w, "  %-10s %-5s %s\n", res.Layer, res.Status, res.Detail)
			if res.Evidence != "" {
				fmt.Fprintf(w, "  %-10s %-5s   %s\n", "", "", res.Evidence)
			}
		}
	}
	return worst
}

// Recorder is the response recorder enabled by --record.
//
// Saved files are NOT copied into testdata AUTOMATICALLY. This is deliberate:
// a recording is a real response and may contain traces of content such as
// file names. A human decides what becomes a fixture; the distributed binary
// isn't inside the source tree either.
type Recorder struct {
	Dir string

	mu    sync.Mutex
	seq   int
	saved []string
	errs  []error
}

// DefaultDir is used when --record-dir isn't given.
const DefaultDir = "recordings"

// For produces a record function for a specific site.
func (r *Recorder) For(siteName string) func(name string, data []byte) {
	return func(name string, data []byte) {
		r.save(siteName, name, data)
	}
}

func (r *Recorder) save(siteName, name string, data []byte) {
	dir := r.Dir
	if dir == "" {
		dir = DefaultDir
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// The timestamp tells which recording is newer when diffing; the sequence
	// number keeps apart answers recorded under the same name within one
	// second (mega records every API answer as api.json), which used to
	// overwrite each other.
	r.seq++
	stamp := time.Now().UTC().Format("20060102-150405")
	fname := fmt.Sprintf("%s-%03d-%s-%s", stamp, r.seq, safe(siteName), safe(name))
	path := filepath.Join(dir, fname)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %w", dir, err))
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %w", path, err))
		return
	}
	r.saved = append(r.saved, path)
}

// Saved returns the paths of the saved files.
func (r *Recorder) Saved() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.saved))
	copy(out, r.saved)
	sort.Strings(out)
	return out
}

// Errs returns the errors that happened while recording.
// A recording error does NOT fail the diagnosis: the real job is the layer
// report, recording is a helper.
func (r *Recorder) Errs() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]error, len(r.errs))
	copy(out, r.errs)
	return out
}

// safe does minimal cleanup for a file name. The full Windows cleanup is in
// dl.Component, but doctor shouldn't depend on it: recording names are short
// labels we produce ourselves, the site name and the file name.
func safe(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "record"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
