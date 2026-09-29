// Package dl is responsible for resumable downloading.
//
// The single source of truth is the `.part.state` file, NOT the size of the
// `.part`. At crash time the two diverge: the operating system may have
// written to the `.part` while the state was not updated. If you trust the
// size, sha256 silently comes out wrong, and that violates the tool's whole
// point: the "no silent failure" principle.
//
// The mirror case also holds and is sneakier: the state can be AHEAD of the
// `.part` (file deleted or shortened). Then resume opens a ZERO hole of offset
// bytes at the start of the file, and the saved hash state PASSES the sha256
// check. That is why the state and the file are compared on every open.
package dl

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// progressInterval is the most frequent interval for progress notifications.
//
// 250 ms is smooth enough for the human eye and sparse enough for the UI.
// Reporting on every write round would mean thousands of updates per second.
const progressInterval = 250 * time.Millisecond

// The suffixes the downloader writes next to the final name. They being fixed
// matters: the file name limit has to reserve room for them (see Download).
const (
	partSuffix  = ".part"
	stateSuffix = ".part.state"
)

// Validator types. Which one is used is stored in the state because If-Range
// accepts both but they mean different things.
const (
	ValidatorNone         = ""
	ValidatorETag         = "etag"
	ValidatorLastModified = "last_modified"
	// ValidatorSHA256: the server names no version (no strong ETag, no
	// Last-Modified) but the site told us the file's SHA-256. It is never
	// sent (there is no If-Range for it); parts fetched at different times
	// are trusted because the hash check over the finished file would catch
	// a mix-up, and a resume only continues while the site still reports the
	// same hash. Why it exists: mediafire's download servers send neither
	// header, so its files could use neither several connections nor resume.
	ValidatorSHA256 = "sha256"
)

// State is the schema of the `.part.state` file.
type State struct {
	Offset        int64  `json:"offset"`
	Validator     string `json:"validator"`
	ValidatorType string `json:"validator_type"`
	SHA256State   []byte `json:"sha256_state"`

	// DecoderState is the stream decoder's (if any) state at Offset. Same
	// logic as the sha256 state: so it can continue where it left off, the
	// decoder's internals are written to disk too. Empty without a decoder.
	DecoderState []byte `json:"decoder_state,omitempty"`

	// Segments are the ranges of a segmented download and each one's
	// progress. When set, Offset is meaningless (0); total progress is the
	// sum of the segments. prepare() resets it on sight so it doesn't mix
	// with the single-stream case.
	Segments []Segment `json:"segments,omitempty"`

	// TotalSize is ONLY the server's Content-Length; -1 if unknown.
	// resumable() looks at it because resume is not attempted on a chunked
	// response (a design decision). The resolver-reported size is kept
	// separately, otherwise chunked downloads would silently become resumable.
	TotalSize int64 `json:"total_size"`

	// ItemSize is the size reported by the resolver; -1 if unknown. It is
	// only a fallback for the "is it complete" check, never used for the
	// resume decision.
	ItemSize int64 `json:"item_size"`
}

func freshState() State { return State{TotalSize: -1, ItemSize: -1} }

// resumable reports whether trying a Range with this state makes sense.
// Without a validator resume is NOT attempted: if the server changed the file
// we'd glue together parts of two different files and never notice.
func (s State) resumable() bool {
	return s.Offset > 0 && s.Validator != "" && s.ValidatorType != ValidatorNone && s.TotalSize >= 0
}

// ifRange is the If-Range value for a Range request: only a validator the
// server gave us; "" otherwise.
func (s State) ifRange() string {
	if s.ValidatorType == ValidatorETag || s.ValidatorType == ValidatorLastModified {
		return s.Validator
	}
	return ""
}

// sameVersion reports whether a state's validator still describes the item
// being downloaded. Only a hash validator can be checked before a request;
// the others are checked by the server through If-Range.
func (s State) sameVersion(it site.Item) bool {
	return s.ValidatorType != ValidatorSHA256 || strings.EqualFold(s.Validator, it.SHA256)
}

// expectedTotal is the size used for the completeness check.
// The server's value comes first; otherwise the resolver-reported size.
func (s State) expectedTotal() int64 {
	if s.TotalSize >= 0 {
		return s.TotalSize
	}
	return s.ItemSize
}

// Reresolver re-resolves a single item when the signed URL expired (403/410).
type Reresolver func(ctx context.Context, sourcePage string) (site.Item, error)

// Classifier classifies an error status code in a site-specific way.
// If it returns nil, dl applies its own default.
//
// Without this hook every 403 counts as "signed URL expired". pixeldrain,
// however, also reports rate limit, hotlink and captcha conditions with 403;
// treating them as an expired URL would mean re-resolving and retrying while
// rate limited.
type Classifier func(resp *http.Response, body []byte) error

type Downloader struct {
	Client *http.Client
	// Logf may be nil.
	Logf func(format string, a ...any)
	// Reresolve may be nil; if nil, 403/410 is a permanent error.
	Reresolve Reresolver
	// Classify may be nil.
	Classify Classifier
	// PrepareURL may be nil. It prepares the URL JUST BEFORE the request.
	//
	// Why it is needed: bunkr's CDN wants a time-limited signature. Signing at
	// resolution time meant signing every file of an album before any
	// download started, and the token of the file at the end of the queue
	// died before its turn came. Because it is called on every ATTEMPT, an
	// expired signature refreshes itself.
	PrepareURL func(ctx context.Context, rawURL string) (string, error)
	// Decode may be nil. It decodes the body BEFORE it is written to disk
	// (mega: AES-CTR). When set, offset, size and sha256 are always in
	// PLAINTEXT terms; CTR preserves length, so Range and Content-Length carry
	// the same numbers.
	Decode func(it site.Item, offset int64, saved []byte, r io.Reader) (site.DecodedStream, error)
	// DecodeRange and NewVerifier may be nil. When set (site.RangeDecoder) a
	// decoded file can be fetched over several connections: every segment
	// decodes its own range and the integrity check runs once over the
	// finished file. Without them a file with a Decode is a single stream.
	DecodeRange func(it site.Item, offset int64, r io.Reader) (io.Reader, error)
	NewVerifier func(it site.Item) (site.Verifier, error)
	// RangeURL may be nil. When set (site.RangeURLer) byte ranges are asked
	// for in the URL instead of with a Range header, and the answer is the
	// range itself. end is exclusive.
	RangeURL func(rawURL string, start, end int64) string
	// Throttle may be nil. The bytes-per-second limit shared by all downloads.
	Throttle *Throttle

	// Segments is the number of parallel connections per file; 0 or 1 means
	// single stream. It drops to 1 by itself when the size is unknown, the
	// server doesn't support Range, the file is smaller than MinSegmentSize,
	// or there is a decoder that can't decode ranges. Set it before the
	// downloader is used; change it later with SetSegments.
	Segments int
	// Connections may be nil. It reports how many connections the file is
	// being fetched over right now; the user asked for Segments, this is what
	// the site ceiling, the host slots and the server actually allowed.
	Connections func(it site.Item, n int)
	// MinSegmentSize is the size below which no splitting happens; 0 = DefaultMinSegmentSize.
	MinSegmentSize int64
	// ChunkSize is the size of the chunks a segmented download is queued in;
	// 0 = chosen from the file size (see chunkSizeFor).
	ChunkSize int64
	// AcquireExtra may be nil (then extra connections are limited only by
	// Segments). Asks for host slots for extra connections: at most want,
	// without waiting. The download's own connection comes with its file
	// slot; this is the host's budget for the ones beyond it.
	AcquireExtra func(rawURL string, want int) (got int, release func())
	// Validate may be nil. It checks in a site-specific way that a successful
	// response REALLY is the requested content.
	//
	// Why the status code isn't enough: for files under maintenance or
	// deleted, bunkr returns a placeholder video with 200, not 404. The
	// status is clean, the content is garbage. Without this hook the tool
	// would count the garbage as "downloaded successfully".
	Validate func(resp *http.Response) error
	// UserAgent is applied to transfer requests too. The resolver's
	// User-Agent only covers API calls, so it has to be given here as well.
	UserAgent string

	// Progress may be nil. Called periodically while a transfer runs.
	//
	// total is -1 if unknown (no Content-Length and the resolver didn't report
	// a size either). The call frequency is deliberately limited: updating the
	// UI every 256 KB of a gigabyte file would lock up the UI.
	Progress func(it site.Item, done, total int64)

	// claimed prevents using the same name twice in the same folder within
	// one run. Duplicate names are common in bunkr albums.
	//
	// Since step 6 Download is called concurrently, so it is locked. Leaving
	// it unlocked would let two items take the same name and one overwrite
	// the other; and being a race, only sometimes.
	mu      sync.Mutex
	claimed map[string]string // lowercased path -> owner (SourcePage)
}

func (d *Downloader) logf(format string, a ...any) {
	if d.Logf != nil {
		d.Logf(format, a...)
	}
}

// SetSegments changes the connections per file while downloads may be
// running; the ones that start next use the new value.
func (d *Downloader) SetSegments(n int) {
	d.mu.Lock()
	d.Segments = n
	d.mu.Unlock()
}

func (d *Downloader) segmentsWanted() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.Segments
}

// conns reports the number of connections in use (silent if Connections is nil).
func (d *Downloader) conns(it site.Item, n int) {
	if d.Connections != nil {
		d.Connections(it, n)
	}
}

// report sends a progress notification (silent if Progress is nil).
func (d *Downloader) report(it site.Item, st State) {
	if d.Progress == nil {
		return
	}
	d.Progress(it, st.Offset, st.expectedTotal())
}

// validate applies the site-specific response validation.
// The error is PERMANENT: maintenance lasts hours and won't clear within a
// 10-minute retry budget; retrying does nothing but spend bandwidth. Unless
// the validator says so itself: a retryable error is retried, and
// site.ErrLinkExpired has the item resolved again like a 403/410.
func (d *Downloader) validate(resp *http.Response) error {
	if d.Validate == nil {
		return nil
	}
	err := d.Validate(resp)
	if errors.Is(err, site.ErrLinkExpired) {
		d.logf("%v", err)
		return &urlExpiredError{status: resp.StatusCode}
	}
	return err
}

func (d *Downloader) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return http.DefaultClient
}

// retryableErr marks an error as worth retrying.
//
// The classification lives NEXT TO the error, not inside the retry loop. That
// way internal/net doesn't have to import dl or site: there is no
// cross-package cycle and "is this error retried" is answered where the error
// is produced.
type retryableErr struct{ err error }

func (e *retryableErr) Error() string   { return e.err.Error() }
func (e *retryableErr) Unwrap() error   { return e.err }
func (e *retryableErr) Retryable() bool { return true }

// Retryable marks an error as retryable.
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return &retryableErr{err: err}
}

// ErrSHA256Mismatch is returned when downloaded content doesn't match the
// hash given by the site.
//
// It is DELIBERATELY not retryable. A hash mismatch is usually deterministic
// (the site's hash is stale, or the content changed), and downloading a
// multi-gigabyte file five times wastes bandwidth. The `.part` is cleaned up,
// so the user can retry by hand.
var ErrSHA256Mismatch = errors.New("sha256 mismatch")

// ErrIntegrity is returned when the stream decoder's own integrity check
// (mega meta-MAC) fails. Like a sha256 mismatch it is NOT retryable: if the
// key is wrong, downloading again produces the same garbage.
var ErrIntegrity = errors.New("integrity check failed")

// ErrIncomplete is returned when the body arrives shorter than expected.
var ErrIncomplete = errors.New("download incomplete")

// Result is the record of a completed download.
//
// The path is returned because the name on disk can differ from the input
// name after passing through the sanitizer. SHA256 is the COMPUTED hash: not
// the one the site gave, but the one produced from the bytes. bunkr gives no
// hash but done.jsonl writes this one so an integrity record is still left.
type Result struct {
	Path   string
	Size   int64
	SHA256 string
}

// Download downloads a single item and manages resumability.
//
// If the error is nil, the file is on disk at Result.Path, verified.
//
// If ctx is canceled, `.part` and `.part.state` are left in a consistent
// state and the context error is returned.
func (d *Downloader) Download(ctx context.Context, outRoot string, it site.Item) (Result, error) {
	// Sanitizing happens HERE, not in the caller: naming rules must live in
	// the same place as the code that writes to disk, otherwise some caller
	// skips it and a reserved device name or a component beyond the 255-unit
	// limit leaks onto disk.
	//
	// It must happen BEFORE claim(): the keys of the name collision map must
	// be the same as the names actually created on disk.
	//
	// The file name is fitted NOT into 255 but into 255 minus our own
	// suffixes: we write `.part` and `.part.state` next to the final name and
	// those files are subject to the same component limit. Spending 255 on
	// the final name pushes `.part.state` to 266 units and NTFS rejects the
	// request. A folder name has no such suffix, so it can use the full limit.
	name := ComponentLimit(it.Filename, MaxComponentUTF16-utf16Len(stateSuffix))
	if name == "" {
		return Result{}, fmt.Errorf("unusable file name: %q (%s)", it.Filename, it.SourcePage)
	}
	it.Filename = name
	it.Dir = DirPath(it.Dir) // may stay empty; means the root

	dir := filepath.Join(outRoot, it.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("could not create folder: %w", err)
	}

	final := filepath.Join(dir, d.claim(dir, it))
	part := final + partSuffix
	statePath := final + stateSuffix

	// If it is already complete, don't touch it. The ledger (done.jsonl) is
	// checked before this; this is only a cheap check against downloading
	// twice.
	if fi, err := os.Stat(final); err == nil && !fi.IsDir() {
		// If the size is known and doesn't match, this is NOT the same file:
		// a file of another album with the same name or the user's own file.
		// Counting it as "already downloaded" would record the wrong content
		// as a success; a permanent error, because retrying finds the same
		// collision.
		if it.Size > 0 && fi.Size() != it.Size {
			return Result{}, fmt.Errorf("a file with the same name but a different size already exists: %s (%d bytes, expected %d); move or delete it, then retry",
				final, fi.Size(), it.Size)
		}
		d.logf("%s already exists, skipping", filepath.Base(final))
		// The hash is not recomputed: the file is already in place and
		// reading a multi-gigabyte file from the start just for the record is
		// an unacceptable cost.
		return Result{Path: final, Size: fi.Size()}, nil
	}

	// The segmented path comes BEFORE prepare(): the single-stream prepare
	// would take a pre-sized .part for "excess" and truncate it. A segmented
	// download left half done is continued segmented even if the user has
	// since dropped to 1 connection; restarting it would throw away what is
	// already on disk.
	if n := d.segmentsFor(it); n > 1 || d.hasSegmentState(statePath) {
		res, serr := d.segmented(ctx, final, part, statePath, it, n)
		if !errors.Is(serr, errNoRangeSupport) {
			return res, serr
		}
		d.logf("%s: downloading as a single stream", filepath.Base(final))
	}

	st, hasher, err := d.prepare(part, statePath, it)
	if err != nil {
		return Result{}, err
	}
	d.conns(it, 1)

	item := it
	tried := false
	var last site.DecodedStream
	for {
		done, newState, ds, aerr := d.attempt(ctx, part, statePath, item, st, hasher)
		if ds != nil {
			last = ds
		}
		if aerr == nil {
			st = newState
			if !done {
				// The server closed early unexpectedly; partial progress is
				// saved, a higher layer will retry.
				return Result{}, Retryable(fmt.Errorf("%w: connection closed early, %d bytes saved",
					ErrIncomplete, st.Offset))
			}
			break
		}

		var expired *urlExpiredError
		if errors.As(aerr, &expired) && d.Reresolve != nil && !tried {
			// The signed URL expired. Re-resolve ONCE.
			tried = true
			d.logf("signed URL returned %d, re-resolving: %s", expired.status, item.SourcePage)
			fresh, rerr := d.Reresolve(ctx, item.SourcePage)
			if rerr != nil {
				return Result{}, fmt.Errorf("re-resolution failed: %w", rerr)
			}
			// Name and folder are kept; the download URL and what goes with
			// it (a session cookie) are refreshed.
			item.URL = fresh.URL
			if fresh.Headers != nil {
				item.Headers = fresh.Headers
			}
			if item.SHA256 == "" {
				item.SHA256 = fresh.SHA256
			}
			// Hash and offset are kept: we continue the same content.
			st, hasher, err = d.prepare(part, statePath, item)
			if err != nil {
				return Result{}, err
			}
			continue
		}
		return Result{}, aerr
	}

	// If the expected size is known, an incomplete file doesn't count as a
	// success. Without Content-Length the resolver-reported size kicks in as
	// a fallback; otherwise a truncated body would count as "complete".
	if total := st.expectedTotal(); total >= 0 && st.Offset != total {
		return Result{}, Retryable(fmt.Errorf("%w: %d/%d bytes (%s)",
			ErrIncomplete, st.Offset, total, filepath.Base(final)))
	}

	// The decoder's own integrity check (mega: meta-MAC). Treated like
	// sha256: a corrupt .part left on disk would repeat the same failure on
	// every run.
	if last != nil {
		if verr := last.Verify(); verr != nil {
			_ = os.Remove(part)
			_ = os.Remove(statePath)
			return Result{}, fmt.Errorf("%w: %v (%s) — .part deleted, can be retried",
				ErrIntegrity, verr, filepath.Base(final))
		}
	}

	sum := hex.EncodeToString(hasher.Sum(nil))
	if item.SHA256 != "" && !strings.EqualFold(sum, item.SHA256) {
		// If a corrupt `.part` is left on disk every run repeats the same
		// failure and the item can't be recovered without deleting it by hand.
		// Clean up and leave it downloadable from scratch.
		_ = os.Remove(part)
		_ = os.Remove(statePath)
		return Result{}, fmt.Errorf("%w: expected %s, computed %s (%s) — .part deleted, can be retried",
			ErrSHA256Mismatch, item.SHA256, sum, filepath.Base(final))
	}
	if err := os.Rename(part, final); err != nil {
		return Result{}, fmt.Errorf("rename: %w", err)
	}
	_ = os.Remove(statePath)
	return Result{Path: final, Size: st.Offset, SHA256: sum}, nil
}

// prepare compares the state with the `.part` file and returns a consistent
// (state, hasher) pair.
//
// There are three inconsistencies and all three can silently produce a
// corrupt file:
//   - no `.part` but state offset > 0   -> resume opens a zero hole at the start
//   - `.part` shorter than the state    -> the same hole, smaller
//   - `.part` longer than the state     -> the excess never entered the hash, truncate it
//
// In the first two the only safe behavior is starting over. Not silently: it
// is logged.
func (d *Downloader) prepare(part, statePath string, it site.Item) (State, hash.Hash, error) {
	st := loadState(statePath)
	if st.ItemSize < 0 && it.Size > 0 {
		st.ItemSize = it.Size
	}

	reset := func(reason string) (State, hash.Hash, error) {
		if reason != "" {
			d.logf("%s: downloading from scratch", reason)
		}
		if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
			return State{}, nil, err
		}
		_ = os.Remove(statePath)
		fresh := freshState()
		if it.Size > 0 {
			fresh.ItemSize = it.Size
		}
		return fresh, sha256.New(), nil
	}

	if len(st.Segments) > 0 {
		// A segmented state cannot be continued as a single stream: the
		// segments are spread across the middle of the file and there is no
		// hash state. Starting over is the only safe path.
		return reset("a segmented state cannot be continued as a single stream")
	}
	if st.Offset <= 0 {
		return reset("")
	}
	// If there is a decoder and the state doesn't carry its state, resume is
	// NOT SAFE: the decoder starts from scratch while the hash and integrity
	// check think they continue from where they stopped. The only correct
	// reaction is to start over.
	if d.Decode != nil && len(st.DecoderState) == 0 {
		return reset("no decoder state, cannot resume")
	}
	if !st.sameVersion(it) {
		return reset("the site now reports a different SHA-256, the file changed")
	}

	fi, err := os.Stat(part)
	switch {
	case os.IsNotExist(err):
		// The sneakiest case: the hash state is in the state file, the file is
		// gone. If resumed, a hole opens and the sha256 check PASSES.
		return reset(fmt.Sprintf("no .part but the state says %d bytes", st.Offset))
	case err != nil:
		return State{}, nil, err
	case fi.Size() < st.Offset:
		return reset(fmt.Sprintf(".part is %d bytes, the state says %d", fi.Size(), st.Offset))
	case fi.Size() > st.Offset:
		// The excess never entered the hash; truncating is right and safe.
		if err := os.Truncate(part, st.Offset); err != nil {
			return State{}, nil, err
		}
	}

	h, err := restoreHasher(part, st)
	if err != nil {
		return reset(fmt.Sprintf("could not restore the hash state: %v", err))
	}
	return st, h, nil
}

// urlExpiredError separates 403/410 from other errors.
type urlExpiredError struct{ status int }

func (e *urlExpiredError) Error() string { return fmt.Sprintf("signed URL invalid: %d", e.status) }

// attempt makes a single HTTP attempt and updates the state.
// done=true means the file is complete.
func (d *Downloader) attempt(
	ctx context.Context, part, statePath string,
	it site.Item, st State, hasher hash.Hash,
) (bool, State, site.DecodedStream, error) {
	// The URL is prepared just before the request (for time-limited parts
	// such as a signature). The error is treated as TRANSIENT: the signing
	// service may be momentarily down; it is not a permanent problem with the
	// file.
	target := it.URL
	if d.PrepareURL != nil {
		prepared, perr := d.PrepareURL(ctx, target)
		if perr != nil {
			return false, st, nil, Retryable(perr)
		}
		target = prepared
	}

	// Continuing through the URL (mega): the site takes the range in the URL.
	// Without a validator that is still safe when there is a decoder, because
	// its integrity check covers the whole file.
	total := st.expectedTotal()
	viaURL := d.RangeURL != nil && st.Offset > 0 && total > st.Offset &&
		((st.Validator != "" && st.ValidatorType != ValidatorNone) || d.Decode != nil)
	reqURL := target
	if viaURL {
		reqURL = d.RangeURL(target, st.Offset, total)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, st, nil, err
	}
	if d.UserAgent != "" {
		req.Header.Set("User-Agent", d.UserAgent)
	}
	// Item.Headers derive from the policy (e.g. bunkr's mandatory item page
	// Referer) and may override User-Agent.
	for k, v := range it.Headers {
		req.Header.Set(k, v)
	}

	// NO separate Accept-Ranges probe: a wasted round trip and not a
	// guarantee. The only correct signal is the status code of the real request.
	if !viaURL && st.resumable() {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(st.Offset, 10)+"-")
		if v := st.ifRange(); v != "" {
			req.Header.Set("If-Range", v)
		}
	}

	resp, err := d.client().Do(req)
	if err != nil {
		// A transport error is treated as transient: a dropped connection, a
		// DNS delay, a TLS handshake timeout. Permanent ones are reported by
		// the HTTP status code.
		return false, st, nil, Retryable(err)
	}
	defer resp.Body.Close()

	status := resp.StatusCode
	if viaURL && (status == http.StatusOK || status == http.StatusPartialContent) {
		switch cl, rest := resp.ContentLength, total-st.Offset; {
		case cl < 0 || cl == rest:
			// The rest of the file, from where the .part stops.
			if verr := d.validate(resp); verr != nil {
				return false, st, nil, verr
			}
			ds, derr := d.decode(it, st.Offset, st.DecoderState, resp.Body)
			if derr != nil {
				return false, st, nil, derr
			}
			return d.stream(ctx, part, statePath, ds, st, hasher, it)
		case cl == total:
			// The server ignored the range and sent the whole file: take it
			// from the start, as with a rejected If-Range.
			status = http.StatusOK
		default:
			return false, st, nil, Retryable(fmt.Errorf("asked for bytes %d-%d in the URL, the server sent %d bytes",
				st.Offset, total-1, cl))
		}
	}

	switch status {
	case http.StatusOK:
		if verr := d.validate(resp); verr != nil {
			return false, st, nil, verr
		}
		// The source changed or the server doesn't support Range: write from scratch.
		if st.Offset > 0 {
			d.logf("server returned 200, the source may have changed: downloading from scratch")
		}
		st = mergeServerState(st, resp, it)
		hasher.Reset()
		if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
			return false, st, nil, err
		}
		st.Offset = 0
		st.DecoderState = nil
		ds, derr := d.decode(it, 0, nil, resp.Body)
		if derr != nil {
			return false, st, nil, derr
		}
		return d.stream(ctx, part, statePath, ds, st, hasher, it)

	case http.StatusPartialContent:
		if verr := d.validate(resp); verr != nil {
			return false, st, nil, verr
		}
		start, total, perr := parseContentRange(resp.Header.Get("Content-Range"))
		if perr != nil {
			return false, st, nil, fmt.Errorf("could not parse Content-Range: %w", perr)
		}
		if start != st.Offset {
			// The server didn't start where we asked. Gluing produces a corrupt file.
			return false, st, nil, fmt.Errorf("Content-Range starts at %d, expected %d", start, st.Offset)
		}
		if total >= 0 {
			st.TotalSize = total
		}
		ds, derr := d.decode(it, st.Offset, st.DecoderState, resp.Body)
		if derr != nil {
			return false, st, nil, derr
		}
		return d.stream(ctx, part, statePath, ds, st, hasher, it)

	case http.StatusRequestedRangeNotSatisfiable:
		// 416 does NOT mean "complete"; it only means "offset >= current
		// length". The state makes the distinction.
		if total := st.expectedTotal(); total >= 0 && st.Offset == total {
			d.logf("416: the download is already complete, verifying")
			return true, st, nil, nil
		}
		d.logf("416 but offset=%d expected=%d: .part is corrupt, resetting", st.Offset, st.expectedTotal())
		if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
			return false, st, nil, err
		}
		_ = os.Remove(statePath)
		hasher.Reset()
		st = freshState()
		st.ItemSize = it.Size
		return false, st, nil, Retryable(errors.New("range rejected, .part reset"))

	case http.StatusForbidden, http.StatusGone:
		// If there is a site-specific classifier ask it first: 403 is not
		// always "expired signed URL".
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		if d.Classify != nil {
			if cerr := d.Classify(resp, body); cerr != nil {
				return false, st, nil, cerr
			}
		}
		return false, st, nil, &urlExpiredError{status: resp.StatusCode}

	default:
		// The site-specific classifier is asked HERE too, not only on
		// 403/410: mega reports its bandwidth quota with 509, and by the
		// generic rule below that would count as "5xx, transient" and be
		// retried again and again.
		if d.Classify != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
			if cerr := d.Classify(resp, body); cerr != nil {
				return false, st, nil, cerr
			}
		}
		err := fmt.Errorf("%s: HTTP %s", it.URL, resp.Status)
		// 5xx and 429 are server side, transient. The rest of 4xx is our
		// fault; retrying brings the same answer.
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return false, st, nil, Retryable(err)
		}
		return false, st, nil, err
	}
}

// decode wraps the body with the decoder if there is one; otherwise it
// returns the body as is, as a stateless, unverified DecodedStream.
func (d *Downloader) decode(it site.Item, offset int64, saved []byte, r io.Reader) (site.DecodedStream, error) {
	if d.Decode == nil {
		return plainStream{r}, nil
	}
	return d.Decode(it, offset, saved, r)
}

// plainStream is the identity transform for sites without a decoder.
type plainStream struct{ io.Reader }

func (plainStream) State() []byte { return nil }
func (plainStream) Verify() error { return nil }

// stream writes the body into the `.part` and advances the hash.
func (d *Downloader) stream(
	ctx context.Context, part, statePath string,
	body site.DecodedStream, st State, hasher hash.Hash, it site.Item,
) (bool, State, site.DecodedStream, error) {
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, st, body, err
	}
	if _, err := f.Seek(st.Offset, io.SeekStart); err != nil {
		f.Close()
		return false, st, body, err
	}

	// The first notification goes out right away: the user must see that the
	// download STARTED; it must not look like nothing happens for the first
	// 250 ms.
	d.report(it, st)
	lastReport := time.Now()
	// The state is written to disk WHILE downloading too (right away on the
	// first write, then every segmentStateEvery). If it were only written on
	// exit, when the process is killed (crash, power cut) the state would
	// never exist or be very old, prepare would cut the excess and all
	// progress would be lost. The segmented path already did this; the
	// single-stream pixeldrain and mega did not.
	var lastSave time.Time

	buf := make([]byte, 256<<10)
	for {
		select {
		case <-ctx.Done():
			_ = f.Sync()
			f.Close()
			saveState(statePath, st, hasher)
			return false, st, body, ctx.Err()
		default:
		}

		// With a limit the chunk shrinks; otherwise the full buffer.
		n, rerr := body.Read(buf[:d.Throttle.chunkFor(len(buf))])
		if n > 0 {
			if terr := d.Throttle.Wait(ctx, n); terr != nil {
				// Canceled while waiting: the chunk read was not written to
				// disk, the offset didn't advance; the state is saved as is.
				_ = f.Sync()
				f.Close()
				saveState(statePath, st, hasher)
				return false, st, body, terr
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				_ = f.Sync()
				f.Close()
				saveState(statePath, st, hasher)
				return false, st, body, werr
			}
			hasher.Write(buf[:n])
			st.Offset += int64(n)
			// The decoder state is taken AT THE SAME TIME as the offset: both
			// must describe the same byte count, otherwise resume decodes from
			// the wrong place.
			st.DecoderState = body.State()
			if time.Since(lastSave) >= segmentStateEvery {
				// Order matters: data to disk first, then the state. The state
				// must never be ahead of the .part (see the package doc); the
				// reverse is safely truncated in prepare.
				if f.Sync() == nil {
					saveState(statePath, st, hasher)
				}
				lastSave = time.Now()
			}
			if time.Since(lastReport) >= progressInterval {
				d.report(it, st)
				lastReport = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = f.Sync()
			f.Close()
			saveState(statePath, st, hasher)
			// A read that breaks mid-body is transient; continue from the saved offset.
			return false, st, body, Retryable(rerr)
		}
	}

	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		saveState(statePath, st, hasher)
		return false, st, body, err
	}
	// Final notification: the last chunk may have been skipped because of the
	// interval, and a progress bar stuck at 98% tells the user "it hung".
	d.report(it, st)

	// If the expected size is known, missing data doesn't silently count as success.
	if total := st.expectedTotal(); total >= 0 && st.Offset < total {
		saveState(statePath, st, hasher)
		return false, st, body, nil
	}
	return true, st, body, nil
}

// mergeServerState takes fresh server information from a 200 response and
// picks the validator. The resolver-reported ItemSize is kept.
//
// A weak ETag (W/ prefix) CANNOT be used in If-Range (RFC 7232): a weak
// validator means "semantically equivalent", not "byte-for-byte identical".
// Using it brings back exactly the corrupt-file class you're trying to close.
func mergeServerState(prev State, resp *http.Response, it site.Item) State {
	st := freshState()
	st.ItemSize = prev.ItemSize
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			st.TotalSize = n
		}
	}
	st.Validator, st.ValidatorType = pickValidator(resp, it)
	return st
}

// pickValidator picks what tells versions of the file apart: a strong ETag,
// otherwise Last-Modified, otherwise the SHA-256 the site reported,
// otherwise none (resume is not attempted).
func pickValidator(resp *http.Response, it site.Item) (string, string) {
	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	switch {
	case etag != "" && !strings.HasPrefix(etag, "W/"):
		return etag, ValidatorETag
	case resp.Header.Get("Last-Modified") != "":
		return resp.Header.Get("Last-Modified"), ValidatorLastModified
	case it.SHA256 != "":
		return strings.ToLower(it.SHA256), ValidatorSHA256
	default:
		return "", ValidatorNone
	}
}

// parseContentRange parses the "bytes 100-199/1234" form.
// If the total is unknown ("*") it returns -1.
func parseContentRange(v string) (start, total int64, err error) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "bytes ") {
		return 0, -1, fmt.Errorf("unexpected format: %q", v)
	}
	v = strings.TrimPrefix(v, "bytes ")
	rangePart, totalPart, ok := strings.Cut(v, "/")
	if !ok {
		return 0, -1, fmt.Errorf("unexpected format: %q", v)
	}
	startStr, _, ok := strings.Cut(rangePart, "-")
	if !ok {
		return 0, -1, fmt.Errorf("unexpected range: %q", rangePart)
	}
	start, err = strconv.ParseInt(strings.TrimSpace(startStr), 10, 64)
	if err != nil {
		return 0, -1, err
	}
	total = -1
	if t := strings.TrimSpace(totalPart); t != "*" {
		total, err = strconv.ParseInt(t, 10, 64)
		if err != nil {
			return 0, -1, err
		}
	}
	return start, total, nil
}

func loadState(path string) State {
	data, err := os.ReadFile(path)
	if err != nil {
		return freshState()
	}
	st := freshState()
	if json.Unmarshal(data, &st) != nil || st.Offset < 0 {
		return freshState()
	}
	return st
}

// saveState writes the state atomically: a half-written state file would
// silently mean a wrong offset on the next run.
func saveState(path string, st State, hasher hash.Hash) {
	if m, ok := hasher.(encoding.BinaryMarshaler); ok {
		if b, err := m.MarshalBinary(); err == nil {
			st.SHA256State = b
		}
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := stateTmpPath(path)
	if os.WriteFile(tmp, data, 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// stateTmpPath is the temp file name for the state's atomic write. It must
// NOT BE LONGER than the state name: the name limit only reserves room for
// ".part.state", and "<name>.part.state.tmp" exceeded 255 units for names at
// the limit and silently failed to be written.
func stateTmpPath(path string) string {
	if strings.HasSuffix(path, stateSuffix) {
		return strings.TrimSuffix(path, stateSuffix) + ".part.stmp"
	}
	return path + ".tmp"
}

// restoreHasher restores the saved sha256 state.
// Without a state it rebuilds the hash by re-reading the `.part` up to offset.
//
// The caller must verify BEFOREHAND that the `.part` is consistent with the
// state (see prepare): here CopyN errors if the file is short, but if the
// hash state comes from the state file the file is never read and the
// inconsistency stays silent.
func restoreHasher(part string, st State) (hash.Hash, error) {
	h := sha256.New()
	if st.Offset == 0 {
		return h, nil
	}
	if len(st.SHA256State) > 0 {
		if u, ok := h.(encoding.BinaryUnmarshaler); ok {
			if err := u.UnmarshalBinary(st.SHA256State); err == nil {
				return h, nil
			}
		}
		h = sha256.New()
	}
	f, err := os.Open(part)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := io.CopyN(h, f, st.Offset); err != nil {
		return nil, err
	}
	return h, nil
}

// claim prevents the same name in the same folder from being used by two
// DIFFERENT items. The suffix derives from Index, so it is deterministic
// across runs.
//
// Ownership is kept by item identity (SourcePage): if the SAME item asks for
// the name a second time it gets the same name back. This is mandatory for
// pause/resume: otherwise a resumed download would veer off to a new name
// with a "(N)" suffix, i.e. another .part, and orphan the half file.
func (d *Downloader) claim(dir string, it site.Item) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.claimed == nil {
		d.claimed = map[string]string{}
	}
	key := func(n string) string { return strings.ToLower(filepath.Join(dir, n)) }
	free := func(n string) bool {
		owner, taken := d.claimed[key(n)]
		return !taken || (owner != "" && owner == it.SourcePage)
	}

	if free(it.Filename) {
		d.claimed[key(it.Filename)] = it.SourcePage
		return it.Filename
	}
	ext := filepath.Ext(it.Filename)
	base := strings.TrimSuffix(it.Filename, ext)
	for n := it.Index + 1; ; n++ {
		alt := fmt.Sprintf("%s (%d)%s", base, n, ext)
		if free(alt) {
			d.claimed[key(alt)] = it.SourcePage
			return alt
		}
	}
}

// Plan tells the final path Download will use BEFORE the download starts.
//
// The queue writes it to disk: when the app is closed and reopened, a
// half-finished job must continue under the same name, not restart its
// ".part" under a new name. Calling it again for the same item is safe;
// claim remembers ownership.
func (d *Downloader) Plan(outRoot string, it site.Item) (string, error) {
	name := ComponentLimit(it.Filename, MaxComponentUTF16-utf16Len(stateSuffix))
	if name == "" {
		return "", fmt.Errorf("unusable file name: %q (%s)", it.Filename, it.SourcePage)
	}
	it.Filename = name
	dir := filepath.Join(outRoot, DirPath(it.Dir))
	return filepath.Join(dir, d.claim(dir, it)), nil
}
