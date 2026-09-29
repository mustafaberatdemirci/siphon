// Package run holds the download pipeline: config loading, resolver
// dispatch, concurrency, retries, the ledger and exit-code accounting.
//
// Why a separate package: there are two interfaces (command line and
// window) and both MUST run the same pipeline. If the pipeline stayed in
// main.go, the window version would have to copy it; two copies drift apart
// and you end up with two programs behaving differently on the same link.
// This package exists solely to make that drift impossible.
//
// It contains nothing interface-specific: no fmt.Println here, no windows.
// What happens is reported outward through Events.
package run

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/mustafaberatdemirci/siphon/internal/config"
	"github.com/mustafaberatdemirci/siphon/internal/dl"
	"github.com/mustafaberatdemirci/siphon/internal/hook"
	snet "github.com/mustafaberatdemirci/siphon/internal/net"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/store"
)

// DefaultMaxInFlight is the number of download goroutines kept alive at the
// same time. The real throttling is done by HostLimiter; this only exists so
// a 5000-item album doesn't open 5000 goroutines.
const DefaultMaxInFlight = 64

// Options is the input of a run.
type Options struct {
	URLs        []string
	OutDir      string
	ConfigPath  string
	ResolveOnly bool
	MaxInFlight int
	// OnQuota is the shell command to run when the site's per-IP quota runs
	// out (e.g. a script that switches VPN server). If empty the quota stops
	// the album. If set: the command runs, QuotaRetryDelay is waited, the same
	// URL is run again; the ledger skips what was downloaded. If the quota
	// hits again the command runs again; at most MaxQuotaRounds rounds.
	OnQuota         string
	QuotaRetryDelay time.Duration // 0 = 3 s (for the tunnel to settle)
}

// MaxQuotaRounds is the per-URL upper bound of the quota command + probe +
// rerun loop. No endless "switch VPN, download another 5 GiB" loop; the user
// can rerun if they want.
const MaxQuotaRounds = 5

// Events reports what happens during a run.
// Every field may be nil; nil ones are silently skipped.
type Events struct {
	// Debugf is detail, Infof normal progress, Errorf errors.
	Debugf func(format string, a ...any)
	Infof  func(format string, a ...any)
	Errorf func(format string, a ...any)

	// ConfigLoaded reports which config was loaded.
	ConfigLoaded func(source string, sites int)
	// LedgerOpened reports the ledger's path and the number of items in it.
	LedgerOpened func(path string, items, skippedLines int)

	// ItemResolved is called for every item in --resolve-only mode.
	ItemResolved func(it site.Item)

	// ItemQueued is called as soon as an item is resolved, as it enters the
	// download queue.
	//
	// It MUST be separate from URLResolved: URLResolved fires when ALL items
	// of a URL are done, so the UI would only learn the total after the work
	// finished and the progress bar would stay at zero for the whole run.
	// That was exactly the bug the user reported.
	ItemQueued func(it site.Item)

	// ItemStarted fires when it leaves the download queue and really starts.
	ItemStarted func(it site.Item)
	// Progress is periodic while a transfer runs. total is -1 if unknown.
	Progress func(it site.Item, done, total int64)
	// Connections reports how many connections the file is being fetched
	// over right now (see dl.Downloader.Connections).
	Connections func(it site.Item, n int)
	// ItemDone fires when the file is verified and moved to its final name.
	ItemDone func(it site.Item, res dl.Result)
	// ItemFailed fires when an item fails permanently.
	ItemFailed func(it site.Item, err error)
	// ItemSkipped fires when the ledger says it is already downloaded.
	ItemSkipped func(it site.Item, e store.Entry)
}

func (e Events) debugf(f string, a ...any) {
	if e.Debugf != nil {
		e.Debugf(f, a...)
	}
}

func (e Events) infof(f string, a ...any) {
	if e.Infof != nil {
		e.Infof(f, a...)
	}
}

func (e Events) errorf(f string, a ...any) {
	if e.Errorf != nil {
		e.Errorf(f, a...)
	}
}

// Summary is the result of a run.
type Summary struct {
	URLs     int // URLs attempted
	Items    int // items resolved
	Done     int // files downloaded
	Failed   int // items that failed permanently
	Skipped  int // skipped because of the ledger
	Degraded int // downloaded but the ledger entry couldn't be written
	// SkippedURLs is the number of URLs skipped because no resolver matched.
	SkippedURLs int
	// ResolvedAny is true if at least one URL produced items.
	ResolvedAny bool
	// Halted is true if the run was interrupted by a signal or a captcha.
	Halted bool
	// ResolveErrors is the number of URL-level resolution errors.
	ResolveErrors int
	// ItemErrors is the number of item-level errors reported by the resolver.
	ItemErrors int
}

// Problems is the number of problems that affect the exit code.
func (s Summary) Problems() int {
	return s.Failed + s.Degraded + s.SkippedURLs + s.ResolveErrors + s.ItemErrors
}

// Exit code contract. 2 and 3 are deliberately separate: 2 means "something
// happened on the site side", 3 means "the input is wrong", and the two have
// completely different diagnosis paths.
const (
	ExitOK      = 0 // every URL resolved and every item downloaded
	ExitPartial = 1 // partial failure, a skipped URL, or interrupted by a signal
	ExitNoneOK  = 2 // no URL could be resolved
	ExitUsage   = 3 // invalid TOML, schema_version mismatch, unreadable input
)

// ExitCode turns the summary into an exit code.
func (s Summary) ExitCode() int {
	switch {
	case s.Halted:
		return ExitPartial
	case !s.ResolvedAny:
		return ExitNoneOK
	case s.Problems() > 0:
		return ExitPartial
	default:
		return ExitOK
	}
}

// ErrUsage is the error class that maps to exit code 3.
var ErrUsage = config.ErrUsage

// Setup loads the config and builds the resolvers.
//
// If canaries isn't empty the config's canary list is OVERRIDDEN (doctor -canary).
// record may be nil; doctor fills it with --record.
//
// client is for the resolvers' API and page requests; if nil a new client
// with timeouts is built. Why it is mandatory: without a client the resolvers
// fall back to http.DefaultClient, which has no response-header timeout; a
// server that doesn't answer locked resolution (and the UI's "Add") forever.
// Giving the same client as the downloader shares the connection pool.
func Setup(
	ev Events, cfgPath string,
	record func(siteName string) func(string, []byte),
	canaries []string,
	client *http.Client,
) ([]site.SiteConfig, []site.Resolver, error) {
	cfgs, src, err := config.Load(config.Embedded, cfgPath)
	if err != nil {
		return nil, nil, err
	}
	if client == nil {
		client = snet.NewClient()
	}
	if ev.ConfigLoaded != nil {
		ev.ConfigLoaded(src.String(), len(cfgs))
	}

	reg := site.NewRegistry()
	for name, factory := range map[string]site.Factory{
		site.PixeldrainName: site.NewPixeldrain,
		site.BunkrName:      site.NewBunkr,
		site.MegaName:       site.NewMega,
		site.DirectName:     site.NewDirect,
	} {
		if rerr := reg.Register(name, factory); rerr != nil {
			return nil, nil, fmt.Errorf("registry: %w", rerr)
		}
	}

	for i := range cfgs {
		cfgs[i].Logf = ev.Debugf
		if cfgs[i].HTTPClient == nil {
			cfgs[i].HTTPClient = client
		}
		if record != nil {
			cfgs[i].Record = record(cfgs[i].Name)
		}
		if len(canaries) > 0 {
			cfgs[i].CanaryURLs = canaries
		}
	}
	resolvers, err := reg.Build(cfgs)
	if err != nil {
		return nil, nil, err
	}

	// The plain-link fallback must stay off the real sites' hosts: a link
	// there that the site doesn't recognize (a mega link with a truncated
	// key, a bunkr CDN URL that needs signing, a mega storage URL) would be
	// saved as a web page or as encrypted bytes. It stays unmatched and is
	// reported as such.
	var owned []string
	for _, c := range cfgs {
		if c.Name == site.DirectName {
			continue
		}
		for _, d := range append(append([]string{}, c.Domains...), c.LegacyDomains...) {
			owned = append(owned, d, "*."+d)
		}
		owned = append(owned, c.MatchPatterns...)
		owned = append(owned, c.CDNPatterns...)
	}
	for _, r := range resolvers {
		if ex, ok := r.(site.HostExcluder); ok {
			ex.ExcludeHosts(owned)
		}
	}
	return cfgs, resolvers, nil
}

// Run resolves and downloads the given URLs.
//
// The returned error is ONLY a usage/configuration error (exit 3).
// Everything on the site side goes into the Summary; this separation is the
// basis of the exit code contract.
func Run(ctx context.Context, opt Options, ev Events) (Summary, error) {
	var sum Summary

	if len(opt.URLs) == 0 {
		return sum, fmt.Errorf("%w: no input", ErrUsage)
	}
	if opt.OutDir == "" {
		opt.OutDir = "."
	}
	inFlight := opt.MaxInFlight
	if inFlight <= 0 {
		inFlight = DefaultMaxInFlight
	}

	// The resolvers and the downloader share the same client (and connection pool).
	httpClient := snet.NewClient()
	cfgs, resolvers, err := Setup(ev, opt.ConfigPath, nil, nil, httpClient)
	if err != nil {
		return sum, err
	}

	// The ledger is opened UNDER THE OUTPUT ROOT, not in the cwd or next to
	// the exe: otherwise a second run with a different output folder would say
	// "everything is downloaded" and return 0 even though the files aren't
	// there.
	//
	// In resolve-only mode it is NOT opened AT ALL: that mode writes nothing
	// to disk, and opening the ledger would mean creating the output folder.
	var ledger *store.Ledger
	if !opt.ResolveOnly {
		l, lerr := store.Open(opt.OutDir)
		if lerr != nil {
			return sum, lerr
		}
		defer l.Close()
		ledger = l
		if ev.LedgerOpened != nil {
			ev.LedgerOpened(l.Path(), l.Len(), l.Skipped())
		}
		if n := l.Skipped(); n > 0 {
			ev.errorf("skipped %d unreadable lines in the ledger: %s", n, l.Path())
		}
	}

	for _, u := range opt.URLs {
		sum.URLs++

		r, idx := Pick(resolvers, u)
		if r == nil {
			// No silent failure: a skipped URL affects the exit code.
			ev.errorf("no matching resolver, skipping: %s", u)
			sum.SkippedURLs++
			continue
		}
		cfg := cfgs[idx]
		rc := runCtx{
			url:      u,
			resolver: r,
			cfg:      cfg,
			client:   httpClient,
			ledger:   ledger,
			opt:      opt,
			ev:       ev,
			inFlight: inFlight,
		}

		res := runURL(ctx, rc, &sum)

		if res.halted {
			sum.Halted = true
			break
		}
		if res.resolveErr != nil {
			ev.errorf("%s: %v", u, res.resolveErr)
			if layer, ok := site.LayerOf(res.resolveErr); ok {
				ev.debugf("  broken layer: %s", layer)
			}
			sum.ResolveErrors++
			continue
		}
		sum.ItemErrors += res.itemErrs
		if res.skipped > 0 {
			ev.infof("%s: %d items (%d already downloaded, skipped)", u, res.count, res.skipped)
		} else {
			ev.infof("%s: %d items", u, res.count)
		}
	}
	return sum, nil
}

// runURL runs a URL and adds the totals to sum. If OnQuota is set it also
// drives the quota loop: the command, the probe, the same URL again (the
// ledger skips what was downloaded). The result of the last round is returned.
func runURL(ctx context.Context, rc runCtx, sum *Summary) oneResult {
	var res oneResult
	for round := 0; ; round++ {
		res = runOne(ctx, rc)
		sum.Items += res.count
		sum.Done += res.done
		sum.Failed += res.failed
		sum.Skipped += res.skipped
		sum.Degraded += res.degraded
		if res.count > 0 {
			sum.ResolvedAny = true
		}
		if !res.quota || rc.opt.OnQuota == "" || round+1 >= MaxQuotaRounds {
			return res
		}
		if !afterQuotaCommand(ctx, rc, round+1) {
			return res
		}
		rc.ev.infof("%s: retrying (%d/%d)", rc.url, round+2, MaxQuotaRounds)
	}
}

// afterQuotaCommand runs the quota command; if it returns true, rerunning is
// worth it. The allowance is not asked of the API (there's no reliable field,
// see site/mega.go); the rerun itself is the probe.
func afterQuotaCommand(ctx context.Context, rc runCtx, round int) bool {
	ev, opt := rc.ev, rc.opt
	ev.infof("quota exceeded, running the command (%d/%d): %s", round, MaxQuotaRounds, opt.OnQuota)
	start := time.Now()
	out, err := hook.Run(ctx, opt.OnQuota, 0)
	if err != nil {
		if out != "" {
			ev.errorf("quota command failed (%v): %s", err, out)
		} else {
			ev.errorf("quota command failed: %v", err)
		}
		return false
	}
	ev.infof("quota command finished (%s)", time.Since(start).Round(time.Second))
	delay := opt.QuotaRetryDelay
	if delay <= 0 {
		delay = 3 * time.Second
	}
	return snet.Sleep(ctx, delay) == nil
}

type runCtx struct {
	url      string
	resolver site.Resolver
	cfg      site.SiteConfig
	client   *http.Client
	ledger   *store.Ledger
	opt      Options
	ev       Events
	inFlight int
}

type oneResult struct {
	count      int
	done       int
	failed     int
	skipped    int
	degraded   int
	itemErrs   int
	halted     bool
	quota      bool // the reason for halted is the quota (for the OnQuota loop)
	resolveErr error
}

// runOne resolves a single URL and downloads its items.
func runOne(ctx context.Context, rc runCtx) oneResult {
	var out oneResult
	r, ev, opt := rc.resolver, rc.ev, rc.opt

	// The Worker is built per URL: the name collision map is album-scoped.
	w := NewWorker(r, rc.cfg, rc.client, ev)

	g, gctx := errgroup.WithContext(ctx)
	// At most max_concurrent files of the site at once, like the window's
	// queue. Each file may open several connections; without this an album
	// spread over many hosts (mega) would open files × connections at once.
	limit := rc.inFlight
	if c := rc.cfg.MaxConcurrent; c > 0 && c < limit {
		limit = c
	}
	g.SetLimit(limit)

	var (
		count    int // Resolve calls from a single goroutine; no atomic needed
		done     atomic.Int64
		failed   atomic.Int64
		already  atomic.Int64
		degrade  atomic.Int64 // item downloaded but its ledger entry couldn't be written
		stopped  atomic.Bool
		quotaHit atomic.Bool
	)

	// Resolve gets the group context too: when a quota or captcha stops the
	// album, resolution must stop as well. On bunkr every item is a separate
	// API call; a context that is never canceled meant pointless requests for
	// the hundreds of files left.
	itemErrs, rerr := r.Resolve(gctx, rc.url, func(it site.Item) error {
		count++
		if opt.ResolveOnly {
			if ev.ItemResolved != nil {
				ev.ItemResolved(it)
			}
			return nil
		}
		// Reported as soon as it is queued: the UI should learn the total as a
		// stream, not wait for the end of the work.
		if ev.ItemQueued != nil {
			ev.ItemQueued(it)
		}
		g.Go(func() error {
			o := w.DownloadItem(gctx, opt.OutDir, rc.ledger, it, ev)
			switch o.Kind {
			case OutcomeDone:
				done.Add(1)
				if o.Degraded {
					degrade.Add(1)
				}
			case OutcomeSkipped:
				already.Add(1)
			case OutcomeFailed:
				failed.Add(1)
				if _, quota := site.QuotaOf(o.Err); quota {
					// The quota is per IP and site wide: the hundreds of items
					// left would get the same 509 in turn. Stop the album; the
					// user reruns when the time passes or after changing IP,
					// and the ledger skips what was downloaded. (If -on-quota
					// was given, Run runs the command and retries itself.)
					ev.errorf("STOPPED: %v", o.Err)
					quotaHit.Store(true)
					stopped.Store(true)
					return o.Err
				}
			case OutcomeStopped:
				stopped.Store(true)
				return o.Err // gctx is canceled, the remaining work stops
			case OutcomeCanceled:
				return o.Err
			}
			return nil
		})
		return nil
	})

	// Resolve returned but downloads may still be running; wait in every case.
	gerr := g.Wait()

	out.count = count
	out.done = int(done.Load())
	out.failed = int(failed.Load())
	out.skipped = int(already.Load())
	out.degraded = int(degrade.Load())
	out.itemErrs = len(itemErrs)

	for _, ie := range itemErrs {
		ev.errorf("item: %v", ie)
	}

	switch {
	case stopped.Load():
		out.halted = true
		out.quota = quotaHit.Load()
	case gerr != nil && errors.Is(gerr, context.Canceled):
		ev.errorf("interrupted: %s", rc.url)
		out.halted = true
	case rerr != nil:
		if errors.Is(rerr, context.Canceled) {
			ev.errorf("interrupted: %s", rc.url)
			out.halted = true
		} else {
			out.resolveErr = rerr
		}
	}
	return out
}

// Pick returns the first resolver that recognizes the URL and its config
// index; nil, -1 if none. Registry.Build preserves order, so the index maps
// one-to-one onto cfgs.
func Pick(rs []site.Resolver, u string) (site.Resolver, int) {
	// Real sites first; a fallback (plain file links) only gets what none of
	// them recognizes, wherever it sits in sites.toml. Otherwise a
	// pixeldrain link could end up downloaded as "a file" (its HTML page).
	for _, fallbacks := range []bool{false, true} {
		for i, r := range rs {
			if isFallback(r) == fallbacks && r.Match(u) {
				return r, i
			}
		}
	}
	return nil, -1
}

func isFallback(r site.Resolver) bool {
	f, ok := r.(site.Fallback)
	return ok && f.Fallback()
}

// ToolPaths returns the tool paths set in the "direct" entry of sites.toml
// (yt_dlp, gallery_dl, ffmpeg, deno) for external.Find; nil if none are set
// or the config can't be read.
func ToolPaths(cfgPath string) map[string]string {
	cfgs, _, err := config.Load(config.Embedded, cfgPath)
	if err != nil {
		return nil
	}
	for _, c := range cfgs {
		if c.Name == site.DirectName {
			return c.Extra
		}
	}
	return nil
}
