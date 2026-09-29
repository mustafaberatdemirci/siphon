// Package site defines Siphon's core contract: producing Items from a site
// and, when something breaks, reporting in a typed way WHICH layer broke.
//
// The layer report is the headline feature, so resolver errors are not plain
// errors: every error is tied to a Layer and doctor reads it with errors.As.
package site

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Layer is a stage at which a resolution attempt can break.
//
// DNS, TLS and Challenge are deliberately SEPARATE layers. A measurement on
// 2026-09-09 showed that each produces a different error signature and needs
// an opposite fix:
//
//	DNS       : the resolved address is outside the expected network -> ISP
//	            block, fix is switching domains. (bunkr.cr -> 2a01:358:... block page)
//	TLS       : handshake or certificate verification fails -> the same block
//	            seen at the TLS layer (x509: unknown authority).
//	Challenge : TLS is clean but 403 + CF-Mitigated: challenge -> Cloudflare,
//	            fix is domain rotation first, utls as a last resort.
//
// Collapsing these into one layer makes doctor a liar: it tells the user
// "TLS problem" when what they actually need is a different domain.
type Layer string

const (
	LayerDNS       Layer = "DNS"
	LayerTLS       Layer = "TLS"
	LayerChallenge Layer = "Challenge"
	LayerFetch     Layer = "Fetch"
	LayerParse     Layer = "Parse"
	LayerItemPage  Layer = "ItemPage"
	LayerCDN       Layer = "CDN"
)

// Layers is doctor's reporting order.
var Layers = []Layer{
	LayerDNS, LayerTLS, LayerChallenge,
	LayerFetch, LayerParse, LayerItemPage, LayerCDN,
}

// LayerError ties an error to a layer. Evidence is raw proof for diagnosis.
type LayerError struct {
	Layer    Layer
	Err      error
	Evidence string // "expected host pattern did not match: kirsch-cdn.ru"
}

func (e *LayerError) Error() string {
	if e.Evidence == "" {
		return fmt.Sprintf("%s: %v", e.Layer, e.Err)
	}
	return fmt.Sprintf("%s: %v (%s)", e.Layer, e.Err, e.Evidence)
}

func (e *LayerError) Unwrap() error { return e.Err }

// Errorf is a shorthand for building a layer-bound error.
func Errorf(l Layer, evidence string, format string, a ...any) *LayerError {
	return &LayerError{Layer: l, Err: fmt.Errorf(format, a...), Evidence: evidence}
}

// LayerOf returns the layer of the first LayerError in the chain.
func LayerOf(err error) (Layer, bool) {
	var le *LayerError
	if errors.As(err, &le) {
		return le.Layer, true
	}
	return "", false
}

// QuotaError says the site's per-IP transfer quota is exhausted.
//
// It is neither permanent nor a "retry right away" error: the file is there,
// access is fine, only this IP's allowance for the current window is used up.
// The right reaction is to wait (or change IP) and then resolve and try again.
// NOT Retryable: the retry policy gives up within minutes while a quota stays
// full for hours; waiting is the queue's job.
//
// Wait is the reset time reported by the site; 0 means unknown.
type QuotaError struct {
	Wait time.Duration
	Err  error
}

func (e *QuotaError) Error() string {
	if e.Wait > 0 {
		return fmt.Sprintf("%v — resets in about %s", e.Err, FormatWait(e.Wait))
	}
	return e.Err.Error()
}

func (e *QuotaError) Unwrap() error { return e.Err }

// QuotaOf returns the QuotaError in the chain.
func QuotaOf(err error) (*QuotaError, bool) {
	var q *QuotaError
	if errors.As(err, &q) {
		return q, true
	}
	return nil, false
}

// FormatWait writes a duration for humans: "5h 6m", "12m".
func FormatWait(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Minute {
		return "1m"
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

type LayerStatus string

const (
	StatusOK   LayerStatus = "OK"
	StatusWarn LayerStatus = "WARN" // e.g. a new CDN host missing from sites.toml
	StatusFail LayerStatus = "FAIL"
)

// LayerResult is one line printed by doctor. Without the WARN/FAIL
// distinction the "CDN is a signal, not a gate" rule cannot be expressed.
type LayerResult struct {
	Layer    Layer
	Status   LayerStatus
	Detail   string // "album selector found 12 items"
	Evidence string // "new host seen: kirsch-cdn.ru"
}

type Item struct {
	URL        string // the real URL to download (CDN, may expire)
	SourcePage string // item page used for re-resolution. Required.
	Dir        string // album folder relative to the output root ("" = root); "/" separates nested folders
	Filename   string
	Headers    map[string]string // Referer included, derived from RefererPolicy
	SHA256     string            // pixeldrain provides it, bunkr does not; may be empty
	Size       int64             // -1 if unknown
	Index      int               // position within the album

	// Secret is resolver-private material; the downloader never touches it,
	// it is never written to the ledger and never logged. For mega it is the
	// content key + nonce + meta-MAC (32 bytes). It travels inside the Item so
	// re-resolution and resume don't have to look it up somewhere else.
	Secret []byte
}

type ItemError struct {
	URL string
	Err error // may wrap a LayerError
}

func (e *ItemError) Error() string { return fmt.Sprintf("%s: %v", e.URL, e.Err) }
func (e *ItemError) Unwrap() error { return e.Err }

// The Referer policy varies per site; it is not a uniform mechanism.
// On pixeldrain a wrong Referer triggers exactly hotlink_detected;
// on bunkr the item page is a mandatory Referer.
const (
	RefererNone     = "none"
	RefererItemPage = "item_page"
	RefererOrigin   = "origin"
)

// SiteConfig defaults.
const (
	DefaultMaxRetries    = 5
	DefaultBaseDelay     = 1 * time.Second
	DefaultMaxDelay      = 60 * time.Second
	DefaultMaxElapsed    = 10 * time.Minute
	DefaultMaxConcurrent = 2
)

type SiteConfig struct {
	Name string

	// Domains is the active domain pool used for matching. Entries may contain
	// wildcards ("bunkr.*"). gallery-dl gave up enumerating TLDs on 2024-08-24
	// and added a wildcard option; keeping a list for bunkr is a lost battle.
	Domains []string

	// LegacyDomains are ACCEPTED when matching URLs but NOT USED for fetching.
	// gallery-dl makes the same distinction with LEGACY_DOMAINS: a link from a
	// dead domain must be recognized, but no request should go to that domain.
	LegacyDomains []string

	// MatchPatterns are wildcard patterns used ONLY for recognition
	// ("bunkr.*"). Same semantics as LegacyDomains: they enable matching but
	// never enter the fetch pool.
	//
	// Why separate: domain rotation needs a concrete list (you cannot pick a
	// random domain from a wildcard), but the list will always be stale.
	// Together: when a new TLD appears the link is recognized and the request
	// goes to a known domain. gallery-dl uses the same pair (BASE_PATTERN + DOMAINS).
	MatchPatterns []string

	CDNPatterns   []string // a signal, not a gate
	UserAgent     string
	RefererPolicy string // none | item_page | origin
	MaxConcurrent int

	// MaxSegments is the maximum number of connections a single file may be
	// fetched with; 1 = segmented download off. It is a CEILING: the user
	// setting cannot exceed it. Per site because gain and risk differ by
	// site: pixeldrain limits concurrent connections per IP on the free tier,
	// bunkr's CDN answers a fourth connection to the same file with 503, mega
	// decrypts every range on its own (site.RangeDecoder).
	MaxSegments int

	// MaxConnections is the most connections open to one host at once: the
	// files' own connections plus their extra ones. MaxConcurrent limits
	// FILES; a file's extra connections come out of what is left and are
	// taken without waiting, so they never keep another file from starting.
	// 0 means MaxConcurrent * MaxSegments.
	MaxConnections int

	// CanaryURLs is a LIST, not a single URL. A measurement showed bunkr.cr
	// blocked by the ISP on this network; a doctor tied to a single canary
	// would have reported a working site as "dead". doctor tries until it
	// finds one that works.
	CanaryURLs []string

	// DNSResolver empty means the system resolver; otherwise a DoH address.
	// It does NOT defeat SNI-based blocking, it only defeats DNS hijacking.
	// Its real value is diagnostic: if the system DNS and DoH disagree, that
	// alone is proof of ISP interference.
	DNSResolver string

	// Retry policy lives in config because Premise 2 promises it.
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	MaxElapsed time.Duration // per item

	// Extra holds site-specific settings (like bunkr's API endpoint).
	//
	// Kept here instead of adding site-specific fields to the shared
	// SiteConfig: bunkr's endpoint has changed three times and is exactly the
	// "config = variables" kind of value, but pixeldrain doesn't care about it.
	Extra map[string]string

	// Record is the response recorder enabled by --record. May be nil.
	//
	// Why it exists: when bunkr breaks and you don't have the REAL response of
	// the broken page, you cannot diff against the old fixture and you end up
	// guessing what changed. Removing that guesswork is this tool's reason to
	// exist.
	Record func(name string, data []byte)

	// Logf is for the resolver's diagnostic lines. May be nil.
	//
	// A logger in a config looks out of place at first, but this tool's
	// headline feature is diagnosability: if domain rotation happens silently
	// the user cannot answer "why is it slow" or "why did it go to another
	// domain". HTTPClient is here for the same reason.
	Logf func(format string, a ...any)

	// HTTPClient for the resolver's API and page requests. Test injection
	// goes through here too: requests are redirected to an httptest.Server by
	// a rewriting RoundTripper attached to this client.
	HTTPClient *http.Client
}

// Logln writes through cfg.Logf if set.
func (c SiteConfig) Logln(format string, a ...any) {
	if c.Logf != nil {
		c.Logf(format, a...)
	}
}

// Recordln records a response through cfg.Record if set.
func (c SiteConfig) Recordln(name string, data []byte) {
	if c.Record != nil && len(data) > 0 {
		c.Record(name, data)
	}
}

// ExtraOr reads a value from Extra; returns def if missing.
func (c SiteConfig) ExtraOr(key, def string) string {
	if v, ok := c.Extra[key]; ok && v != "" {
		return v
	}
	return def
}

// WithDefaults fills zero-valued fields with their defaults.
func (c SiteConfig) WithDefaults() SiteConfig {
	if c.MaxRetries == 0 {
		c.MaxRetries = DefaultMaxRetries
	}
	if c.BaseDelay == 0 {
		c.BaseDelay = DefaultBaseDelay
	}
	if c.MaxDelay == 0 {
		c.MaxDelay = DefaultMaxDelay
	}
	if c.MaxElapsed == 0 {
		c.MaxElapsed = DefaultMaxElapsed
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = DefaultMaxConcurrent
	}
	if c.MaxSegments == 0 {
		c.MaxSegments = 1
	}
	// Unset: every file may use its full share of connections at once.
	// Never below one per file: a file's own connection comes with its slot.
	if c.MaxConnections == 0 {
		c.MaxConnections = c.MaxConcurrent * c.MaxSegments
	}
	if c.MaxConnections < c.MaxConcurrent {
		c.MaxConnections = c.MaxConcurrent
	}
	if c.RefererPolicy == "" {
		c.RefererPolicy = RefererNone
	}
	return c
}

type Resolver interface {
	Match(u string) bool

	// Resolve calls yield for every item as it is found. Partial success is
	// expressible: "900 resolved, 100 failed" is returned via []ItemError.
	Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error)

	// ResolveOne re-resolves a single item when its signed CDN URL gets a
	// 403/410. Premise 4 depends on it. sourcePage equals Item.SourcePage.
	ResolveOne(ctx context.Context, sourcePage string) (Item, error)

	// Diagnose produces a layer report against the canary.
	Diagnose(ctx context.Context) ([]LayerResult, error)
}

// StatusClassifier lets a resolver classify HTTP error statuses in a
// site-specific way. The downloader uses it optionally (via type assertion),
// so it does not widen the Resolver interface.
//
// Why it is needed: on its own the downloader treats 403 as "signed URL
// expired". pixeldrain, however, also reports rate limit, hotlink and captcha
// conditions with 403. Without this distinction the tool would re-resolve the
// URL and try again while rate limited, deepening the limit with its own hands.
//
// Returning nil means "not recognized, apply the default".
type StatusClassifier interface {
	ClassifyStatus(resp *http.Response, body []byte) error
}

// ResponseValidator lets a resolver validate a download response in a
// site-specific way. The downloader uses it optionally.
//
// Why it is needed: for deleted or maintenance files bunkr serves a
// placeholder video (maint.mp4) with 200 instead of 404. The status is clean,
// the content is garbage. Without this hook the tool would count the garbage
// as "downloaded successfully", which is the worst kind of silent corruption.
//
// Returning nil means "the response is valid".
type ResponseValidator interface {
	ValidateResponse(resp *http.Response) error
}

// URLPreparer is an optional interface for resolvers that must prepare the
// download URL JUST BEFORE the request. The downloader uses it via type
// assertion.
//
// Why it is needed: bunkr's CDN wants a signed URL and the signature expires
// (2 hours). Signing at resolution time meant signing every file of an album
// before any download started; the token of the file at the end of the queue
// died before its turn came. Files that were skipped or only listed also got
// signatures for nothing.
//
// It is called again on every attempt, so an expired token refreshes itself
// and the item doesn't need to be resolved from scratch.
type URLPreparer interface {
	PrepareURL(ctx context.Context, rawURL string) (string, error)
}

// ErrLinkExpired is what a ResponseValidator wraps when the answer shows the
// download link, or what it needs alongside (a session cookie), is no longer
// valid: the downloader then resolves the item again once, as it does for a
// 403/410, and takes both the new URL and the new headers.
//
// Why it is needed: gofile answers a download whose session token expired
// with a redirect to its web page, a 200 with HTML. By status alone that is
// neither an expired link nor an error.
var ErrLinkExpired = errors.New("the download link expired")

// StreamDecoder is an optional interface for resolvers whose body must be
// decoded BEFORE it is written to disk. The downloader uses it via type
// assertion.
//
// Why it is needed: mega files are encrypted client-side. Handing over a URL
// and saying "download" would write encrypted garbage to disk; the key sits
// after the # in the link and never reaches the server. For pixeldrain and
// bunkr the URL was enough, here it is not.
//
// offset is the plaintext byte the stream starts at (resume). saved is the
// decoder state left from a previous run; may be empty. If the decoder cannot
// restore its state while offset > 0 it must return an ERROR and must not
// silently start over: the integrity check relies on that state.
type StreamDecoder interface {
	DecodeStream(it Item, offset int64, saved []byte, r io.Reader) (DecodedStream, error)
}

// DecodedStream is the decoded body. The downloader hashes and writes to disk
// through it, so offset, size and sha256 state are always in PLAINTEXT terms.
type DecodedStream interface {
	io.Reader
	// State returns the decoder state to save for resume. It must correspond
	// exactly to the number of bytes read so far; the downloader takes it
	// after every write and stores it in the state file.
	State() []byte
	// Verify checks integrity once the stream is COMPLETE. If it fails the
	// downloader deletes the .part: leaving corrupt content on disk would
	// repeat the same failure on every run.
	Verify() error
}

// RangeDecoder is an optional interface for StreamDecoder resolvers whose
// body can be decoded starting at ANY offset without earlier state, and
// whose integrity can be checked afterwards over the finished plaintext.
// With it the downloader fetches a decoded file over several connections:
// every segment decodes its own range, and the integrity check runs once
// over the complete file.
//
// Why it is needed: mega's content is AES-CTR (any block can be decrypted on
// its own) but its integrity MAC is a chain over the whole file in order. A
// StreamDecoder carries that chain along a single stream, so a mega file used
// to be limited to one connection whatever the user chose.
type RangeDecoder interface {
	// DecodeRange decodes r, whose first byte is plaintext byte offset.
	DecodeRange(it Item, offset int64, r io.Reader) (io.Reader, error)
	// NewVerifier returns a checker that is fed the finished plaintext from
	// the first byte to the last.
	NewVerifier(it Item) (Verifier, error)
}

// Verifier checks integrity over plaintext written to it in order.
type Verifier interface {
	io.Writer
	Verify() error
}

// RangeURLer is an optional interface for sites whose servers take byte
// ranges in the URL instead of a Range header; the answer is the range
// itself. end is exclusive.
//
// Why it is needed: mega's own clients, MegaBasterd and go-mega all ask
// mega's storage servers for ".../<start>-<end>". Whether those servers honor
// a Range header is not verified; without this, multiple connections and
// resuming would rest on that guess.
type RangeURLer interface {
	RangeURL(rawURL string, start, end int64) string
}

// Factory injects the config into a resolver.
type Factory func(cfg SiteConfig) Resolver

// Registry holds no global state; every test builds its own instance.
type Registry struct {
	factories map[string]Factory
}

func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

func (r *Registry) Register(name string, f Factory) error {
	if name == "" {
		return errors.New("site: registration with an empty name")
	}
	if f == nil {
		return fmt.Errorf("site: nil factory for %q", name)
	}
	if _, dup := r.factories[name]; dup {
		return fmt.Errorf("site: %q is already registered", name)
	}
	r.factories[name] = f
	return nil
}

// Build calls the registered factory for every SiteConfig and returns the
// resolver list.
func (r *Registry) Build(cfgs []SiteConfig) ([]Resolver, error) {
	out := make([]Resolver, 0, len(cfgs))
	for _, cfg := range cfgs {
		f, ok := r.factories[cfg.Name]
		if !ok {
			return nil, fmt.Errorf("site: no factory registered for %q", cfg.Name)
		}
		out = append(out, f(cfg.WithDefaults()))
	}
	return out, nil
}
