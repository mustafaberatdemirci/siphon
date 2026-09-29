package dl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// Segmented multi-connection download: a single file is split into chunks
// kept in a queue, and every connection takes the next free chunk as soon as
// it finishes one, writing it at its own position in a pre-sized .part file.
//
// Why a queue and not one range per connection: connections don't run at the
// same speed. With N fixed ranges the fast connections finish early and close,
// and the end of the file comes down over one or two slow ones. With a queue
// the fast connections simply take more chunks, so all of them stay busy
// until the last few chunks. MegaBasterd downloads mega files the same way.
//
// When it does NOT kick in (falls back to a single stream by itself):
//   - the size is unknown (no Content-Length): ranges can't be built
//   - the server ignores the range request
//   - the file is smaller than MinSegmentSize: splitting costs more than it gains
//   - there is a stream decoder that can only decode from the start
//     (Decode without DecodeRange)
//   - a single-stream .part is half done: it finishes as a single stream
//
// Integrity: sha256 is a sequential hash, so once the chunks finish the file
// is read FROM THE START and hashed. One full local read; seconds on an SSD,
// and far cheaper than silently counting it as "correct". A decoder's own
// integrity check (mega's meta-MAC, also a chain over the whole file) runs in
// the same pass.

// Segment is a byte range of the file (a chunk of the queue) and how much of
// it has been downloaded.
type Segment struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"` // exclusive
	Done  int64 `json:"done"`
}

// DefaultMinSegmentSize is the file size below which no splitting happens.
const DefaultMinSegmentSize = 8 << 20

// Chunk sizing: large enough that the cost of one more request is noise,
// small enough that every connection gets several chunks (and the fast ones
// can take more). MegaBasterd uses 20 MB.
const (
	chunksPerConn = 4
	minChunkSize  = 4 << 20
	maxChunkSize  = 32 << 20
)

// segmentStateEvery is the most frequent interval at which download state is
// written to disk (segmented and single-stream alike).
const segmentStateEvery = time.Second

// errNoRangeSupport says the server ignored the range request; fall back to a
// single stream.
var errNoRangeSupport = errors.New("server does not support Range")

// errSourceChanged says the server rejected If-Range while continuing and
// returned 200: the segments no longer belong to the same version.
var errSourceChanged = errors.New("source changed, segmented state is invalid")

// errYield: the server complained about the number of connections and the
// pool lowered its limit; the connection hands its chunk back so another one
// (or itself, if it is still allowed) takes it.
var errYield = errors.New("chunk handed back")

// Per-chunk retry limits. The outer policy (Worker) already retries the whole
// download; this is so one chunk's transient error doesn't bring down the
// other connections.
const (
	segmentTries       = 6
	segmentBackoffBase = 500 * time.Millisecond
	segmentBackoffMax  = 8 * time.Second
)

// chunkSizeFor picks the chunk size of a fresh download: about chunksPerConn
// chunks per connection, within [minChunkSize, maxChunkSize]. A file too
// small for that gets one chunk per connection instead, so every connection
// still has work. fixed > 0 overrides it (tests).
func chunkSizeFor(size int64, want int, fixed int64) int64 {
	if fixed > 0 {
		return fixed
	}
	if want < 1 {
		want = 1
	}
	c := size / int64(want*chunksPerConn)
	if c < minChunkSize {
		c = minChunkSize
	}
	if c > maxChunkSize {
		c = maxChunkSize
	}
	if (size+c-1)/c < int64(want) {
		c = (size + int64(want) - 1) / int64(want)
	}
	if c < 1 {
		c = 1
	}
	return c
}

// planChunks splits [0,size) into chunks of the given size; the last one takes the remainder.
func planChunks(size, chunk int64) []Segment {
	if chunk < 1 || size <= chunk {
		return []Segment{{Start: 0, End: size}}
	}
	out := make([]Segment, 0, (size+chunk-1)/chunk)
	for start := int64(0); start < size; start += chunk {
		end := start + chunk
		if end > size {
			end = size
		}
		out = append(out, Segment{Start: start, End: end})
	}
	return out
}

// planSegments splits [0,size) into n equal segments. The last one takes the remainder.
func planSegments(size int64, n int) []Segment {
	if n < 1 {
		n = 1
	}
	if int64(n) > size {
		n = int(size)
	}
	if n < 1 {
		return []Segment{{Start: 0, End: size}}
	}
	each := size / int64(n)
	out := make([]Segment, n)
	for i := range out {
		out[i].Start = int64(i) * each
		out[i].End = out[i].Start + each
	}
	out[n-1].End = size
	return out
}

// segmentsFor is the effective number of segments for this item.
func (d *Downloader) segmentsFor(it site.Item) int {
	n := d.segmentsWanted()
	if !d.canSegment() || n < 2 {
		return 1
	}
	min := d.MinSegmentSize
	if min <= 0 {
		min = DefaultMinSegmentSize
	}
	if it.Size > 0 && it.Size < min {
		return 1
	}
	return n
}

// canSegment: a decoder that can only decode from the start rules out
// segments; one that decodes any range (DecodeRange) doesn't.
func (d *Downloader) canSegment() bool {
	return d.Decode == nil || d.DecodeRange != nil
}

// hasSegmentState reports whether a segmented download of this file was left half done.
func (d *Downloader) hasSegmentState(statePath string) bool {
	return d.canSegment() && len(loadState(statePath).Segments) > 0
}

// versionSafe: can segments fetched at different times be trusted to belong
// to the same version of the file? A validator guarantees it (If-Range on
// every request); without one, a decoder's integrity check over the finished
// file catches a mix-up instead. Why the second path: it is NOT verified that
// mega's storage servers send an ETag or Last-Modified, and the content of a
// mega file never changes under the same key anyway.
func (d *Downloader) versionSafe(validator, vtype string) bool {
	return (validator != "" && vtype != ValidatorNone) || d.NewVerifier != nil
}

// segmented runs a segmented download from start to finish.
//
// Returns: (result, nil) done; (Result{}, errNoRangeSupport) fall back to a
// single stream; any other error: the caller (policy) retries, the segment
// state is on disk.
func (d *Downloader) segmented(ctx context.Context, final, part, statePath string, it site.Item, want int) (Result, error) {
	st := loadState(statePath)

	// Is there a segmented state we can continue?
	resuming := false
	if len(st.Segments) > 0 {
		fi, err := os.Stat(part)
		switch {
		case err != nil, fi.Size() != st.TotalSize, !d.versionSafe(st.Validator, st.ValidatorType):
			// File missing, size doesn't match or no way to tell versions apart: from scratch.
			d.logf("segmented state unusable, downloading from scratch")
			st = freshState()
		default:
			resuming = true
		}
	} else if st.Offset > 0 {
		// A half-done single-stream file: it is not continued segmented, the
		// caller finishes it as a single stream.
		return Result{}, errNoRangeSupport
	}
	if !resuming && want < 2 {
		// Called only to continue a segmented state, and there is none left
		// to continue.
		return Result{}, errNoRangeSupport
	}

	item := it
	reresolved := false
	for {
		res, err := d.segmentedOnce(ctx, final, part, statePath, item, want, &st, resuming)
		var expired *urlExpiredError
		if errors.As(err, &expired) && d.Reresolve != nil && !reresolved {
			reresolved = true
			d.logf("signed URL returned %d, re-resolving: %s", expired.status, item.SourcePage)
			fresh, rerr := d.Reresolve(ctx, item.SourcePage)
			if rerr != nil {
				return Result{}, fmt.Errorf("re-resolution failed: %w", rerr)
			}
			item.URL = fresh.URL
			if item.SHA256 == "" {
				item.SHA256 = fresh.SHA256
			}
			resuming = len(st.Segments) > 0
			continue
		}
		return res, err
	}
}

// chunkPool hands the chunks of one download out to its connections. Its
// mutex also guards the chunks' Done counters, which the connections update
// as they write.
type chunkPool struct {
	mu   sync.Mutex
	segs []Segment
	busy []bool // a connection is fetching this chunk right now
	// limit is how many connections the server tolerates; it starts
	// unlimited and drops when the server answers 503/429.
	limit int
	// running is the number of connections alive.
	running int
}

func newChunkPool(segs []Segment) *chunkPool {
	return &chunkPool{segs: segs, busy: make([]bool, len(segs)), limit: math.MaxInt32}
}

func (p *chunkPool) allowed(want int) int {
	if want < 1 {
		want = 1
	}
	if p.limit < want {
		return p.limit
	}
	return want
}

// take gives the calling connection the next free chunk. If there is none,
// or there are more connections than allowed now (the server pushed back or
// the user lowered the number), the connection is told to close; the count
// drops in the same step so that two connections can't both decide to leave.
func (p *chunkPool) take(want int) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running > p.allowed(want) {
		p.running--
		return -1, false
	}
	for i := range p.segs {
		if !p.busy[i] && p.segs[i].Done < p.segs[i].End-p.segs[i].Start {
			p.busy[i] = true
			return i, true
		}
	}
	p.running--
	return -1, false
}

// release hands a chunk back (finished, failed or yielded).
func (p *chunkPool) release(i int) {
	p.mu.Lock()
	p.busy[i] = false
	p.mu.Unlock()
}

// free is the number of chunks nobody has taken yet; under the lock.
func (p *chunkPool) free() int {
	n := 0
	for i := range p.segs {
		if !p.busy[i] && p.segs[i].Done < p.segs[i].End-p.segs[i].Start {
			n++
		}
	}
	return n
}

// wantsMore reports whether one more connection would have work to do and is allowed.
func (p *chunkPool) wantsMore(want int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running < p.allowed(want) && p.free() > 0
}

// join counts a new connection in, re-checking wantsMore under the lock.
func (p *chunkPool) join(want int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running >= p.allowed(want) || p.free() == 0 {
		return false
	}
	p.running++
	return true
}

// shrink lowers the limit to one below the connections in use (at least 1).
// True if it dropped.
func (p *chunkPool) shrink() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.running
	if p.limit < cur {
		cur = p.limit
	}
	if cur <= 1 {
		return cur, false
	}
	p.limit = cur - 1
	return p.limit, true
}

func (d *Downloader) segmentedOnce(ctx context.Context, final, part, statePath string, it site.Item, want int, st *State, resuming bool) (Result, error) {
	target := it.URL
	if d.PrepareURL != nil {
		prepared, perr := d.PrepareURL(ctx, target)
		if perr != nil {
			return Result{}, Retryable(perr)
		}
		target = prepared
	}

	if !resuming {
		// Probe: size, validator and range support in one small request.
		size, validator, vtype, err := d.probe(ctx, target, it)
		if err != nil {
			return Result{}, err
		}
		if size <= 0 || !d.versionSafe(validator, vtype) {
			// A file without a size, or without a way to tell versions apart,
			// can't be segmented: we couldn't guarantee the segments belong
			// to the same version.
			return Result{}, errNoRangeSupport
		}
		min := d.MinSegmentSize
		if min <= 0 {
			min = DefaultMinSegmentSize
		}
		if size < min {
			return Result{}, errNoRangeSupport
		}
		*st = freshState()
		st.TotalSize, st.ItemSize = size, it.Size
		st.Validator, st.ValidatorType = validator, vtype
		st.Segments = planChunks(size, chunkSizeFor(size, want, d.ChunkSize))

		f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return Result{}, err
		}
		if err := f.Truncate(size); err != nil {
			f.Close()
			return Result{}, err
		}
		f.Close()
		saveState(statePath, *st, nil)
	}

	f, err := os.OpenFile(part, os.O_WRONLY, 0o644)
	if err != nil {
		return Result{}, err
	}
	// Windows won't rename an open file: it must be closed BEFORE hashing and
	// renaming. closeOnce closes it exactly once on every path.
	var closeOnce sync.Once
	closeF := func() { closeOnce.Do(func() { _ = f.Sync(); _ = f.Close() }) }
	defer closeF()

	p := newChunkPool(st.Segments)
	// The number of connections follows the user's CURRENT choice, not the
	// one the download started with: raising it adds connections to running
	// downloads, lowering it closes some after their chunk.
	wantNow := func() int {
		if n := d.segmentsWanted(); n >= 1 {
			return n
		}
		return 1
	}
	if want < 2 {
		// Resuming a segmented state with segmenting now turned down.
		wantNow = func() int { return 1 }
	}

	var (
		lastSave  = time.Now()
		lastRep   = time.Now()
		lastConns = -1
		total     = st.TotalSize
		doneBytes = func() int64 {
			var n int64
			for _, s := range p.segs {
				n += s.Done
			}
			return n
		}
	)
	snapshot := func() State {
		c := *st
		c.Segments = append([]Segment(nil), p.segs...)
		c.Offset = 0
		return c
	}
	// tick is called after every chunk write: it reports progress and
	// periodically writes the chunk state to disk. Saving must happen WHILE
	// DOWNLOADING; if it were only written at the end, a process that dies
	// midway would start everything from scratch.
	tick := func(force bool) {
		p.mu.Lock()
		now := time.Now()
		doRep := force || now.Sub(lastRep) >= progressInterval
		doSave := force || now.Sub(lastSave) >= segmentStateEvery
		if doRep {
			lastRep = now
		}
		if doSave {
			lastSave = now
		}
		n := doneBytes()
		var snap State
		if doSave {
			snap = snapshot()
		}
		// The connection count goes out only when it changed (connections
		// join and leave, the server makes us shrink); rare enough that it
		// needs no rate limit of its own.
		conns := -1
		if c := p.running; c != lastConns {
			lastConns, conns = c, c
		}
		p.mu.Unlock()
		if doSave {
			saveState(statePath, snap, nil)
		}
		if doRep && d.Progress != nil {
			d.Progress(it, n, total)
		}
		if conns >= 0 {
			d.conns(it, conns)
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	var conn func(releaseSlot func()) error
	// grow opens more connections while there is work for them and the
	// user's number, the server and the host's connection budget allow it.
	// Called at the start and after every chunk: a slot freed by another
	// download is picked up while this one is still running.
	grow := func() {
		for p.wantsMore(wantNow()) {
			releaseSlot := func() {}
			if d.AcquireExtra != nil {
				var got int
				got, releaseSlot = d.AcquireExtra(target, 1)
				if got == 0 {
					return // the host's connection budget is used up
				}
			}
			if !p.join(wantNow()) {
				releaseSlot()
				return
			}
			g.Go(func() error { return conn(releaseSlot) })
		}
	}
	conn = func(releaseSlot func()) error {
		defer releaseSlot()
		for {
			i, ok := p.take(wantNow())
			if !ok {
				tick(false)
				return nil
			}
			err := d.runChunk(gctx, p, i, target, it, *st, f, tick)
			p.release(i)
			if errors.Is(err, errYield) {
				continue
			}
			if err != nil {
				return err
			}
			grow()
		}
	}

	// The download's own connection comes with its file slot (the caller's
	// host limit); every further one is an extra.
	p.running = 1
	tick(true)
	g.Go(func() error { return conn(func() {}) })
	grow()
	d.logf("%s: segmented download, %d chunks, %s", filepath.Base(final), len(p.segs), humanSize(st.TotalSize))

	gerr := g.Wait()
	closeF()
	tick(true)

	if errors.Is(gerr, errSourceChanged) {
		// The server changed the file: the old segments belong to another
		// version. If the state isn't deleted the next attempt gets the same
		// 200 with the same If-Range and spins until the budget runs out.
		_ = os.Remove(part)
		_ = os.Remove(statePath)
		return Result{}, Retryable(gerr)
	}
	if gerr != nil {
		return Result{}, gerr
	}
	if got := doneBytes(); got != total {
		return Result{}, Retryable(fmt.Errorf("%w: %d/%d bytes (%s)", ErrIncomplete, got, total, filepath.Base(final)))
	}

	// Integrity: sha256 is sequential, the file is read from the start. A
	// decoder's integrity check (mega's meta-MAC) rides along in the same read.
	var verifier site.Verifier
	if d.NewVerifier != nil {
		v, verr := d.NewVerifier(it)
		if verr != nil {
			return Result{}, verr
		}
		verifier = v
	}
	sum, err := hashFile(part, verifier)
	if err != nil {
		return Result{}, err
	}
	if verifier != nil {
		if verr := verifier.Verify(); verr != nil {
			_ = os.Remove(part)
			_ = os.Remove(statePath)
			return Result{}, fmt.Errorf("%w: %v (%s) — .part deleted, can be retried",
				ErrIntegrity, verr, filepath.Base(final))
		}
	}
	if it.SHA256 != "" && !strings.EqualFold(sum, it.SHA256) {
		_ = os.Remove(part)
		_ = os.Remove(statePath)
		return Result{}, fmt.Errorf("%w: expected %s, computed %s (%s) — .part deleted, can be retried",
			ErrSHA256Mismatch, it.SHA256, sum, filepath.Base(final))
	}
	if err := os.Rename(part, final); err != nil {
		return Result{}, fmt.Errorf("rename: %w", err)
	}
	_ = os.Remove(statePath)
	return Result{Path: final, Size: total, SHA256: sum}, nil
}

// firstByte asks for the file's first byte: in the URL when the site takes
// ranges there (RangeURL), otherwise with a Range header. It is the smallest
// request that is still a real transfer: the segmented download's probe and
// the queue's "has the quota opened" check both use it. The body (at most
// 16 KB of it) comes back already read.
func (d *Downloader) firstByte(ctx context.Context, target string, it site.Item) (*http.Response, []byte, error) {
	reqURL := target
	if d.RangeURL != nil {
		reqURL = d.RangeURL(target, 0, 1)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, nil, err
	}
	d.setHeaders(req, it)
	if d.RangeURL == nil {
		req.Header.Set("Range", "bytes=0-0")
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return nil, nil, Retryable(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	return resp, body, nil
}

// probe learns the size, validator and range support by asking for the first byte.
func (d *Downloader) probe(ctx context.Context, target string, it site.Item) (size int64, validator, vtype string, err error) {
	resp, body, err := d.firstByte(ctx, target, it)
	if err != nil {
		return 0, "", "", err
	}
	switch {
	case d.RangeURL != nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent):
		// The range was in the URL: exactly one byte must come back. More
		// means the server ignored it and sent the file from the start.
		if len(body) != 1 {
			return 0, "", "", errNoRangeSupport
		}
		if verr := d.validate(resp); verr != nil {
			return 0, "", "", verr
		}
		size = it.Size
		if _, total, perr := parseContentRange(resp.Header.Get("Content-Range")); perr == nil && total > 0 {
			size = total
		}
		v, vt := pickValidator(resp)
		return size, v, vt, nil
	case resp.StatusCode == http.StatusPartialContent:
		if verr := d.validate(resp); verr != nil {
			return 0, "", "", verr
		}
		_, total, perr := parseContentRange(resp.Header.Get("Content-Range"))
		if perr != nil || total <= 0 {
			return 0, "", "", errNoRangeSupport
		}
		v, vt := pickValidator(resp)
		return total, v, vt, nil
	case resp.StatusCode == http.StatusOK:
		// The range was ignored.
		return 0, "", "", errNoRangeSupport
	default:
		return 0, "", "", d.classifyStatus(resp, body)
	}
}

// CheckAccess reports whether the item can be transferred right now, using
// the smallest request that is still a real transfer: its first byte. nil
// means yes. Otherwise the error is classified like a download's: a quota
// error (site.QuotaOf) means the quota is still exhausted, IsURLExpired means
// the URL has to be resolved again (mega binds it to the IP that asked for
// it, so a VPN switch expires it).
//
// The queue calls it every few seconds while a site's quota is exhausted, so
// that a VPN switch is noticed right away without downloading a whole file
// to find out.
func (d *Downloader) CheckAccess(ctx context.Context, it site.Item) error {
	target := it.URL
	if d.PrepareURL != nil {
		prepared, perr := d.PrepareURL(ctx, target)
		if perr != nil {
			return Retryable(perr)
		}
		target = prepared
	}
	resp, body, err := d.firstByte(ctx, target, it)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
		return nil
	}
	return d.classifyStatus(resp, body)
}

// IsURLExpired reports whether err says the item's URL is no longer valid
// and has to be resolved again (403/410 that the site didn't classify
// otherwise).
func IsURLExpired(err error) bool {
	var e *urlExpiredError
	return errors.As(err, &e)
}

// runChunk drives a chunk until it is done: on a transient error it waits
// and retries. If the server complains about the number of connections
// (503/429) the pool's limit drops and the chunk is handed back (errYield):
// one connection fewer, the chunk is taken again. Permanent errors (source
// changed, URL expired, 4xx) return right away and bring down the group.
func (d *Downloader) runChunk(ctx context.Context, p *chunkPool, i int, target string, it site.Item, st State, f *os.File, tick func(bool)) error {
	for attempt := 0; ; attempt++ {
		err := d.fetchSegment(ctx, target, it, st, f, &p.mu, &p.segs[i], tick)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var rt retryableError
		if !errors.As(err, &rt) || !rt.Retryable() || attempt+1 >= segmentTries {
			return err
		}
		if isOverloaded(err) {
			if n, ok := p.shrink(); ok {
				d.logf("%s: server complains about the number of connections (%v); parallelism lowered to %d",
					filepath.Base(it.Filename), firstLineOf(err), n)
				return errYield
			}
		}
		wait := segmentBackoffBase << uint(attempt)
		if wait > segmentBackoffMax {
			wait = segmentBackoffMax
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

type retryableError interface{ Retryable() bool }

// overloadedError covers the cases where the server says "too many
// connections": 503 and 429.
type overloadedError struct{ status int }

func (e *overloadedError) Error() string   { return fmt.Sprintf("HTTP %d", e.status) }
func (e *overloadedError) Retryable() bool { return true }
func isOverloaded(err error) bool          { var o *overloadedError; return errors.As(err, &o) }
func firstLineOf(err error) string         { return firstLine(err.Error()) }

// firstLine is the first line of a multi-line error message (so a log line stays one line).
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// fetchSegment fetches a single range from where it left off and writes it to the file.
func (d *Downloader) fetchSegment(ctx context.Context, target string, it site.Item, st State, f *os.File, mu *sync.Mutex, seg *Segment, report func(bool)) error {
	mu.Lock()
	start := seg.Start + seg.Done
	end := seg.End
	mu.Unlock()
	if start >= end {
		return nil
	}

	reqURL := target
	if d.RangeURL != nil {
		reqURL = d.RangeURL(target, start, end)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	d.setHeaders(req, it)
	if d.RangeURL == nil {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end-1))
		if st.Validator != "" {
			// The segments MUST belong to the same version; if the server
			// changed the file it returns 200, caught below.
			req.Header.Set("If-Range", st.Validator)
		}
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return Retryable(err)
	}
	defer resp.Body.Close()

	switch {
	case d.RangeURL != nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent):
		// The range was in the URL; the body must be exactly that range.
		// Writing anything else at this position would corrupt the file.
		if resp.ContentLength >= 0 && resp.ContentLength != end-start {
			return fmt.Errorf("segment %d-%d: asked for %d bytes in the URL, the server sent %d",
				start, end-1, end-start, resp.ContentLength)
		}
	case resp.StatusCode == http.StatusPartialContent:
		gotStart, _, perr := parseContentRange(resp.Header.Get("Content-Range"))
		if perr != nil || gotStart != start {
			return fmt.Errorf("segment %d-%d: Content-Range starts at %d", start, end-1, gotStart)
		}
	case resp.StatusCode == http.StatusOK:
		// The source changed or Range was ignored: mixing segments produces a
		// corrupt file. The caller deletes the state and starts over.
		return errSourceChanged
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		return fmt.Errorf("segment %d-%d: 416", start, end-1)
	default:
		return d.classifyFailure(resp)
	}

	// A decoded file (mega): every segment decodes its own range, starting
	// at its own plaintext offset.
	var body io.Reader = resp.Body
	if d.DecodeRange != nil {
		dec, derr := d.DecodeRange(it, start, resp.Body)
		if derr != nil {
			return derr
		}
		body = dec
	}

	buf := make([]byte, 256<<10)
	pos := start
	for pos < end {
		want := len(buf)
		if rem := end - pos; rem < int64(want) {
			want = int(rem)
		}
		want = d.Throttle.chunkFor(want)
		n, rerr := body.Read(buf[:want])
		if n > 0 {
			if terr := d.Throttle.Wait(ctx, n); terr != nil {
				return terr
			}
			if _, werr := f.WriteAt(buf[:n], pos); werr != nil {
				return werr
			}
			pos += int64(n)
			mu.Lock()
			seg.Done = pos - seg.Start
			mu.Unlock()
			report(false)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return Retryable(rerr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if pos < end {
		return Retryable(fmt.Errorf("%w: segment %d-%d cut off at byte %d", ErrIncomplete, start, end-1, pos))
	}
	return nil
}

// classifyFailure turns non-2xx statuses of segment requests into errors.
func (d *Downloader) classifyFailure(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	return d.classifyStatus(resp, body)
}

// classifyStatus: same rules as in attempt: the site's classifier first;
// 403/410 "URL expired"; 503/429 overloaded (transient, and a signal to lower
// parallelism); other 5xx transient; other 4xx permanent.
func (d *Downloader) classifyStatus(resp *http.Response, body []byte) error {
	if d.Classify != nil {
		if cerr := d.Classify(resp, body); cerr != nil {
			return cerr
		}
	}
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone:
		return &urlExpiredError{status: resp.StatusCode}
	case resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests:
		return &overloadedError{status: resp.StatusCode}
	case resp.StatusCode >= 500:
		return Retryable(fmt.Errorf("HTTP %s", resp.Status))
	default:
		return fmt.Errorf("HTTP %s", resp.Status)
	}
}

// setHeaders applies the item's headers and the User-Agent to the request.
func (d *Downloader) setHeaders(req *http.Request, it site.Item) {
	if d.UserAgent != "" {
		req.Header.Set("User-Agent", d.UserAgent)
	}
	for k, v := range it.Headers {
		req.Header.Set(k, v)
	}
}

// hashFile computes a file's sha256 by reading it from the start. If also is
// non-nil it is fed the same bytes (a decoder's integrity check).
func hashFile(path string, also io.Writer) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	var w io.Writer = h
	if also != nil {
		w = io.MultiWriter(h, also)
	}
	if _, err := io.Copy(w, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// humanSize is a rough size for logs.
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
