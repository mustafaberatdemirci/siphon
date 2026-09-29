package run

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mustafaberatdemirci/siphon/internal/dl"
	snet "github.com/mustafaberatdemirci/siphon/internal/net"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/store"
)

// Worker holds the download components for a single site together: the
// downloader, the per-host concurrency limit and the retry policy.
//
// Why a separate type: there are two callers. The command line does a batch
// run (resolve a URL, download its items, done); the window drives a
// persistent queue (items start, pause and resume one by one). Both MUST
// download a single item the SAME way: ledger check, host limit, policy,
// ledger write. If this logic lived in two places it would drift apart over time.
type Worker struct {
	Resolver site.Resolver
	Cfg      site.SiteConfig
	Down     *dl.Downloader
	Limiter  *snet.HostLimiter
	Policy   snet.Policy
}

// NewWorker builds a worker, wiring the resolver's optional interfaces into the downloader.
func NewWorker(r site.Resolver, cfg site.SiteConfig, client *http.Client, ev Events) *Worker {
	down := &dl.Downloader{
		Client:      client,
		Logf:        ev.Debugf,
		Reresolve:   r.ResolveOne,
		UserAgent:   cfg.UserAgent,
		Progress:    ev.Progress,
		Connections: ev.Connections,
	}
	// If the resolver can interpret 403 in a site-specific way, wire it into
	// the downloader; otherwise every 403 counts as "signed URL expired" and
	// the rate limit deepens.
	if c, ok := r.(site.StatusClassifier); ok {
		down.Classify = c.ClassifyStatus
	}
	// If the URL must be prepared just before the request (a time-limited
	// signature on bunkr), it is wired into the downloader.
	if p, ok := r.(site.URLPreparer); ok {
		down.PrepareURL = p.PrepareURL
	}
	// If the body must be decoded before it is written to disk (mega: AES-CTR).
	if dec, ok := r.(site.StreamDecoder); ok {
		down.Decode = dec.DecodeStream
	}
	// If every range can be decoded on its own (mega: AES-CTR), a decoded file
	// can be fetched over several connections too.
	if rd, ok := r.(site.RangeDecoder); ok {
		down.DecodeRange = rd.DecodeRange
		down.NewVerifier = rd.NewVerifier
	}
	// bunkr can return a maintenance placeholder with 200; the status code
	// isn't a sufficient signal.
	if v, ok := r.(site.ResponseValidator); ok {
		down.Validate = v.ValidateResponse
	}

	// mega's storage servers take byte ranges in the URL.
	if ru, ok := r.(site.RangeURLer); ok {
		down.RangeURL = ru.RangeURL
	}

	// The limits are PER HOST: an album can spread across several CDN hosts
	// and a single global counter would throttle in the wrong place.
	//
	// Files and connections are counted separately. A file takes a file slot
	// (max_concurrent) and comes with its own connection; its extra
	// connections come from the host's remaining budget (max_connections
	// minus the file slots), taken without waiting. Before, both came out of
	// one pool: a file that took every slot for its segments kept the next
	// file waiting at 0% until it finished.
	limiter := snet.NewHostLimiter(cfg.MaxConcurrent)
	down.Segments = cfg.MaxSegments
	if extra := cfg.MaxConnections - cfg.MaxConcurrent; extra > 0 {
		extras := snet.NewHostLimiter(extra)
		down.AcquireExtra = func(rawURL string, want int) (int, func()) {
			return extras.TryAcquire(snet.HostOf(rawURL), want)
		}
	} else {
		down.AcquireExtra = func(string, int) (int, func()) { return 0, func() {} }
	}

	return &Worker{
		Resolver: r,
		Cfg:      cfg,
		Down:     down,
		Limiter:  limiter,
		Policy: snet.Policy{
			MaxAttempts: cfg.MaxRetries,
			MaxElapsed:  cfg.MaxElapsed,
			Backoff:     snet.Backoff{Base: cfg.BaseDelay, Max: cfg.MaxDelay},
			Logf:        ev.Debugf,
		},
	}
}

// OutcomeKind is how a single item ended.
type OutcomeKind int

const (
	OutcomeDone     OutcomeKind = iota // downloaded and recorded
	OutcomeSkipped                     // the ledger already had it, the file is in place
	OutcomeFailed                      // permanent failure
	OutcomeStopped                     // captcha: the run must stop
	OutcomeCanceled                    // context cancellation (the user stopped it)
)

// Outcome is the result of DownloadItem.
type Outcome struct {
	Kind   OutcomeKind
	Result dl.Result   // on Done
	Entry  store.Entry // on Skipped
	Err    error       // on Failed/Stopped/Canceled
	// Degraded: the file was downloaded but its ledger entry couldn't be
	// written. The download is valid, idempotence is broken; the next run
	// downloads it again.
	Degraded bool
}

// DownloadItem handles a single item from start to finish: ledger check,
// host limit, download under the policy, ledger write. It reports events
// through ev.
//
// ledger may be nil (a caller that keeps no ledger); then the skip check and
// the ledger write are not done.
func (w *Worker) DownloadItem(ctx context.Context, outDir string, ledger *store.Ledger, it site.Item, ev Events) Outcome {
	// Already downloaded? The ledger ALONE is not TRUSTED: it is also checked
	// that the file really is in place. If the user deleted the file, looking
	// at the ledger and skipping would be a silent failure.
	if ledger != nil {
		if e, ok := ledger.Lookup(it.Dir, it.SourcePage, it.Filename); ok {
			if fi, serr := os.Stat(filepath.Join(outDir, e.Path)); serr == nil && !fi.IsDir() {
				if ev.ItemSkipped != nil {
					ev.ItemSkipped(it, e)
				}
				ev.debugf("[%d] %s already in the ledger, skipping", it.Index+1, e.Filename)
				return Outcome{Kind: OutcomeSkipped, Entry: e}
			}
			ev.errorf("  the ledger says %q but the file is missing, downloading again", e.Path)
		}
	}

	release, aerr := w.Limiter.Acquire(ctx, snet.HostOf(it.URL))
	if aerr != nil {
		return Outcome{Kind: OutcomeCanceled, Err: aerr}
	}
	defer release()

	if ev.ItemStarted != nil {
		ev.ItemStarted(it)
	}

	var res dl.Result
	derr := w.Policy.Do(ctx, func(int) error {
		var e error
		res, e = w.Down.Download(ctx, outDir, it)
		return e
	})

	switch {
	case derr == nil:
		out := Outcome{Kind: OutcomeDone, Result: res}
		if ledger != nil {
			// The path is recorded RELATIVE: so the ledger stays valid when
			// the output folder is moved.
			rel, relErr := filepath.Rel(outDir, res.Path)
			if relErr != nil {
				rel = filepath.Base(res.Path)
			}
			if lerr := ledger.Add(store.Entry{
				SourcePage: it.SourcePage,
				Dir:        it.Dir,
				Filename:   it.Filename,
				Path:       rel,
				Size:       res.Size,
				SHA256:     res.SHA256,
			}); lerr != nil {
				ev.errorf("  %s downloaded but its ledger entry could not be written: %v", rel, lerr)
				out.Degraded = true
			}
		}
		if ev.ItemDone != nil {
			ev.ItemDone(it, res)
		}
		ev.infof("[%d] %s OK", it.Index+1, filepath.Base(res.Path))
		return out

	case errors.Is(derr, snet.ErrStop):
		// Captcha. Waiting doesn't solve it and continuing makes things
		// worse; stop the run.
		ev.errorf("STOPPED: %v", derr)
		return Outcome{Kind: OutcomeStopped, Err: derr}

	case errors.Is(derr, context.Canceled):
		return Outcome{Kind: OutcomeCanceled, Err: derr}

	default:
		// A dead item inside an album doesn't bring the album down.
		ev.errorf("  %s: %v", it.Filename, derr)
		if ev.ItemFailed != nil {
			ev.ItemFailed(it, derr)
		}
		return Outcome{Kind: OutcomeFailed, Err: derr}
	}
}
