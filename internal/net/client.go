// Package net provides the HTTP client factory, a per-host concurrency limit
// and exponential backoff with jitter.
//
// This is where Premise 3 becomes concrete: a rate limit is not an error, it
// is a normal operating state. pixeldrain enforces concurrent connection and
// transfer limits on the free tier; the tool must expect to hit them and back
// off, otherwise it deepens the limit with its own hands.
package net

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// NewClient builds the client for transfer and API requests.
//
// Client.Timeout is DELIBERATELY not set: that field also covers reading the
// body and would cut a multi-gigabyte download in the middle. Timeouts are
// kept at the connection and response-header level; a hung server is caught,
// a slow but working transfer is not cut.
func NewClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			// A segmented download fetches chunk after chunk over the same
			// connections; an idle pool smaller than the connections to a
			// host (mega: up to 32) would close them between chunks and
			// pay a new TLS handshake for every chunk.
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// IsTransient reports whether the error is of the "the network can't be
// reached right now" kind: DNS did not resolve, the connection could not be
// established or dropped, a timeout. These can be a temporary state of the
// local network (Wi-Fi connecting, VPN switching) and fix themselves after a
// while; they must not be treated as permanent failures.
//
// The user's cancellation (context.Canceled) and real server answers (404,
// "deleted") are NOT transient.
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var nerr net.Error
	return errors.As(err, &nerr) && nerr.Timeout()
}

// HostLimiter limits the number of concurrent requests per host.
//
// The limit is PER HOST, not per run: an album can spread over several CDN
// hosts (normal behavior on bunkr), and a single global counter would
// throttle in the wrong place. Opening 30 concurrent connections to one host
// instead of 3 means max_concurrent_downloads on pixeldrain, right away.
//
// x/sync/semaphore was not used: a buffered channel per host is this much code.
type HostLimiter struct {
	n  int
	mu sync.Mutex
	ch map[string]chan struct{}
}

func NewHostLimiter(n int) *HostLimiter {
	if n < 1 {
		n = 1
	}
	return &HostLimiter{n: n, ch: make(map[string]chan struct{})}
}

// HostOf returns the limiter key of a URL. Unparseable URLs fall into a
// single bucket; that beats silently leaving them unlimited.
func HostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "?"
	}
	return u.Host
}

func (l *HostLimiter) slot(host string) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.ch[host]
	if !ok {
		c = make(chan struct{}, l.n)
		l.ch[host] = c
	}
	return c
}

// TryAcquire takes AT MOST want slots for the host, without waiting. It
// returns how many it got and a function that releases all of them.
//
// For segmented downloads: a file's extra connections come out of the
// host's connection budget (max_connections minus the file slots), and only
// when free slots exist: waiting for one would hold up the download that
// could run on the connection it already has.
func (l *HostLimiter) TryAcquire(host string, want int) (int, func()) {
	c := l.slot(host)
	got := 0
	for got < want {
		select {
		case c <- struct{}{}:
			got++
		default:
			want = got
		}
	}
	n := got
	var once sync.Once
	return n, func() {
		once.Do(func() {
			for i := 0; i < n; i++ {
				<-c
			}
		})
	}
}

// Acquire takes one slot for the host. The returned function releases the
// slot and must be called (defer).
func (l *HostLimiter) Acquire(ctx context.Context, host string) (func(), error) {
	c := l.slot(host)
	select {
	case c <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-c }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Backoff produces exponential backoff with jitter.
//
// cenkalti/backoff was not used: this much code isn't worth a dependency and
// a v4/v5 migration.
type Backoff struct {
	Base time.Duration // first delay
	Max  time.Duration // upper bound
}

// Delay returns the delay for attempt (0-based).
//
// Half jitter is used: the delay is chosen from [d/2, d), with d growing
// exponentially. Full jitter ([0, d)) can produce values near zero and waste
// an attempt; a fixed delay brings requests that hit the limit together back
// together and re-forms the herd. Half jitter sits between the two: progress
// is guaranteed, concurrency is spread.
func (b Backoff) Delay(attempt int) time.Duration {
	base := b.Base
	if base <= 0 {
		base = time.Second
	}
	max := b.Max
	if max <= 0 {
		max = 60 * time.Second
	}
	if attempt < 0 {
		attempt = 0
	}

	d := base
	for i := 0; i < attempt && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int64N(int64(half)))
}

// Sleep waits while respecting ctx.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ErrStop reports that the whole run must stop.
// Captcha falls into this class: waiting doesn't solve it, the user has to be
// told, and by scope we don't try to solve captchas.
var ErrStop = errors.New("stopping the run")

// ErrExhausted is returned when the retry budget is used up. The item is a
// "permanent failure".
var ErrExhausted = errors.New("retry budget exhausted")

// These two interfaces let errors carry their own retry semantics. That way
// the net package doesn't have to import site or dl; there is no cycle and
// the classification lives next to the error.
type retryableError interface{ Retryable() bool }
type captchaError interface{ CaptchaRequired() bool }

// Policy is the per-item retry budget.
//
// There are two separate upper bounds and both are needed: MaxAttempts guards
// against fast, repeating errors, MaxElapsed against errors where each attempt
// takes a long time. With only one of them, a slow error would burn hours in
// 5 attempts.
type Policy struct {
	MaxAttempts int
	MaxElapsed  time.Duration
	Backoff     Backoff
	// Logf may be nil.
	Logf func(format string, a ...any)
}

func (p Policy) logf(format string, a ...any) {
	if p.Logf != nil {
		p.Logf(format, a...)
	}
}

// Do tries op until the budget runs out or a permanent error arrives.
//
// Classification order matters: captcha comes before everything, because
// waiting doesn't solve it and continuing makes things worse.
func (p Policy) Do(ctx context.Context, op func(attempt int) error) error {
	attempts := p.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	deadline := time.Time{}
	if p.MaxElapsed > 0 {
		deadline = time.Now().Add(p.MaxElapsed)
	}

	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op(attempt)
		if err == nil {
			return nil
		}
		last = err

		// Context cancellation is not retried; the user asked us to stop.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		var capt captchaError
		if errors.As(err, &capt) && capt.CaptchaRequired() {
			return fmt.Errorf("%w: captcha required, not attempting to solve it: %w", ErrStop, err)
		}

		var rt retryableError
		if !errors.As(err, &rt) || !rt.Retryable() {
			return err // permanent
		}

		if attempt == attempts-1 {
			break
		}
		d := p.Backoff.Delay(attempt)
		if !deadline.IsZero() && time.Now().Add(d).After(deadline) {
			p.logf("budget exhausted (%s), not waiting", p.MaxElapsed)
			break
		}
		p.logf("attempt %d/%d failed (%v), retrying in %s", attempt+1, attempts, err, d.Round(time.Millisecond))
		if serr := Sleep(ctx, d); serr != nil {
			return serr
		}
	}
	return fmt.Errorf("%w (%d attempts): %w", ErrExhausted, attempts, last)
}
