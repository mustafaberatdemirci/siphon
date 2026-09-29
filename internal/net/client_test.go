package net

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewClientHasNoOverallTimeout(t *testing.T) {
	c := NewClient()
	// Client.Timeout also covers reading the body; it would cut a
	// multi-gigabyte download in the middle. Leaving this field EMPTY is a
	// deliberate decision.
	if c.Timeout != 0 {
		t.Fatalf("Client.Timeout = %v, should be 0 (it also covers reading the body)", c.Timeout)
	}
}

// IsTransient must recognize "network is unavailable right now" errors; it
// must not recognize the user's cancellation or real server answers (404,
// deleted).
func TestIsTransient(t *testing.T) {
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connectex: network is unreachable")}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"dns", &net.DNSError{Err: "no such host", Name: "x"}, true},
		{"wrapped dns", &url.Error{Op: "Get", URL: "https://x", Err: &net.DNSError{Err: "no such host"}}, true},
		{"connection", dial, true},
		{"layered connection", fmt.Errorf("re-resolve: %w", &url.Error{Op: "Get", URL: "u", Err: dial}), true},
		{"timeout", &url.Error{Op: "Get", URL: "u", Err: timeoutErr{}}, true},
		{"canceled", &url.Error{Op: "Get", URL: "u", Err: context.Canceled}, false},
		{"server answer", errors.New("API 400: file deleted"), false},
	}
	for _, c := range cases {
		if got := IsTransient(c.err); got != c.want {
			t.Errorf("%s: IsTransient = %v, want %v", c.name, got, c.want)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestHostLimiterCapsConcurrencyPerHost(t *testing.T) {
	const limit = 2
	const workers = 12
	l := NewHostLimiter(limit)

	var live, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := l.Acquire(context.Background(), "example.test")
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			defer release()
			n := live.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			live.Add(-1)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > limit {
		t.Fatalf("peak concurrency %d, limit %d", got, limit)
	}
	if peak.Load() < 2 {
		t.Error("no concurrency happened at all; the test is meaningless")
	}
}

// The limit is PER HOST: different hosts must not block each other.
// Otherwise there would be needless throttling across an album's CDN hosts.
func TestHostLimiterIsPerHost(t *testing.T) {
	l := NewHostLimiter(1)
	rel1, err := l.Acquire(context.Background(), "a.test")
	if err != nil {
		t.Fatal(err)
	}
	defer rel1()

	// Same host is full: a short ctx must time out right away.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx, "a.test"); err == nil {
		t.Error("a second slot was handed out for the same host despite a limit of 1")
	}

	// Another host must be free.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	rel2, err := l.Acquire(ctx2, "b.test")
	if err != nil {
		t.Fatalf("a different host was blocked: %v", err)
	}
	rel2()
}

func TestHostLimiterReleaseIsIdempotent(t *testing.T) {
	l := NewHostLimiter(1)
	release, err := l.Acquire(context.Background(), "a.test")
	if err != nil {
		t.Fatal(err)
	}
	release()
	release() // calling it twice must not corrupt the slot count

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r2, err := l.Acquire(ctx, "a.test")
	if err != nil {
		t.Fatalf("slot could not be reacquired: %v", err)
	}
	r2()
}

func TestHostOf(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://pixeldrain.com/api/file/x", "pixeldrain.com"},
		{"https://cdn.bunkr.ws:8443/a/b", "cdn.bunkr.ws:8443"},
		{"broken url", "?"},
		{"", "?"},
	}
	for _, c := range cases {
		if got := HostOf(c.in); got != c.want {
			t.Errorf("HostOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	b := Backoff{Base: 100 * time.Millisecond, Max: 2 * time.Second}

	// Half jitter: the delay is in [d/2, d). Never zero, because zero wastes
	// an attempt.
	for attempt := 0; attempt < 12; attempt++ {
		d := b.Delay(attempt)
		if d <= 0 {
			t.Fatalf("Delay(%d) = %v; zero wastes an attempt", attempt, d)
		}
		if d > b.Max {
			t.Fatalf("Delay(%d) = %v, cap %v", attempt, d, b.Max)
		}
	}

	// Growth: the average of early attempts must be smaller than later ones.
	avg := func(attempt int) time.Duration {
		var total time.Duration
		for i := 0; i < 200; i++ {
			total += b.Delay(attempt)
		}
		return total / 200
	}
	if avg(0) >= avg(3) {
		t.Errorf("no exponential growth: attempt0=%v attempt3=%v", avg(0), avg(3))
	}
}

func TestBackoffJitterSpreadsValues(t *testing.T) {
	b := Backoff{Base: time.Second, Max: time.Minute}
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[b.Delay(2)] = true
	}
	// A fixed delay brings requests that hit the limit together back together
	// and re-forms the herd. Jitter exists to spread them.
	if len(seen) < 10 {
		t.Fatalf("only %d distinct values produced; jitter is not working", len(seen))
	}
}

func TestBackoffZeroValuesUseDefaults(t *testing.T) {
	var b Backoff
	d := b.Delay(0)
	if d <= 0 || d > time.Minute {
		t.Fatalf("defaults are not reasonable: %v", d)
	}
}

// --- Policy ---

type retryableTestErr struct{ n int }

func (e *retryableTestErr) Error() string   { return fmt.Sprintf("transient %d", e.n) }
func (e *retryableTestErr) Retryable() bool { return true }

type captchaTestErr struct{}

func (captchaTestErr) Error() string         { return "captcha required" }
func (captchaTestErr) Retryable() bool       { return false }
func (captchaTestErr) CaptchaRequired() bool { return true }

func fastPolicy(attempts int) Policy {
	return Policy{
		MaxAttempts: attempts,
		Backoff:     Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond},
	}
}

func TestPolicyRetriesThenExhausts(t *testing.T) {
	calls := 0
	err := fastPolicy(4).Do(context.Background(), func(attempt int) error {
		calls++
		return &retryableTestErr{n: attempt}
	})
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("expected ErrExhausted: %v", err)
	}
	if calls != 4 {
		t.Fatalf("%d attempts made, want 4", calls)
	}
}

func TestPolicySucceedsAfterRetry(t *testing.T) {
	calls := 0
	err := fastPolicy(5).Do(context.Background(), func(int) error {
		calls++
		if calls < 3 {
			return &retryableTestErr{}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("should have succeeded: %v", err)
	}
	if calls != 3 {
		t.Fatalf("%d attempts, want 3", calls)
	}
}

// A permanent error is not retried: it brings the same answer and wastes bandwidth.
func TestPolicyPermanentErrorIsNotRetried(t *testing.T) {
	permanent := errors.New("404 not found")
	calls := 0
	err := fastPolicy(5).Do(context.Background(), func(int) error {
		calls++
		return permanent
	})
	if !errors.Is(err, permanent) {
		t.Fatalf("the permanent error should be returned as is: %v", err)
	}
	if calls != 1 {
		t.Fatalf("%d attempts made, a permanent error must be tried once", calls)
	}
}

// Captcha comes before everything: waiting doesn't solve it and continuing
// makes things worse. By scope we don't solve captchas.
func TestPolicyCaptchaStopsImmediately(t *testing.T) {
	calls := 0
	err := fastPolicy(5).Do(context.Background(), func(int) error {
		calls++
		return captchaTestErr{}
	})
	if !errors.Is(err, ErrStop) {
		t.Fatalf("expected ErrStop: %v", err)
	}
	if calls != 1 {
		t.Fatalf("%d attempts made, a captcha must be tried once", calls)
	}
}

func TestPolicyContextCancelStopsRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Policy{
		MaxAttempts: 10,
		Backoff:     Backoff{Base: 50 * time.Millisecond, Max: time.Second},
	}.Do(ctx, func(int) error {
		calls++
		if calls == 2 {
			cancel()
		}
		return &retryableTestErr{}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled: %v", err)
	}
	if calls > 3 {
		t.Errorf("%d attempts made after cancellation", calls)
	}
}

// Without MaxElapsed a slow error would burn hours over 5 attempts.
func TestPolicyMaxElapsedCutsWaiting(t *testing.T) {
	start := time.Now()
	calls := 0
	err := Policy{
		MaxAttempts: 20,
		MaxElapsed:  80 * time.Millisecond,
		Backoff:     Backoff{Base: 60 * time.Millisecond, Max: time.Second},
	}.Do(context.Background(), func(int) error {
		calls++
		return &retryableTestErr{}
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("expected ErrExhausted: %v", err)
	}
	if calls >= 20 {
		t.Errorf("the budget never kicked in: %d attempts", calls)
	}
	if elapsed > 2*time.Second {
		t.Errorf("the budget was exceeded: %v", elapsed)
	}
}

func TestPolicyZeroAttemptsRunsOnce(t *testing.T) {
	calls := 0
	_ = Policy{}.Do(context.Background(), func(int) error {
		calls++
		return &retryableTestErr{}
	})
	if calls != 1 {
		t.Fatalf("%d attempts, want 1 with a zero configuration", calls)
	}
}

func TestSleepRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("must not wait on a canceled ctx: %v", err)
	}
}
