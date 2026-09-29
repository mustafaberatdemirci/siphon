package dl

import (
	"context"
	"sync"
	"time"
)

// Throttle is a bytes-per-second limit shared by all downloads. Token bucket:
// the bucket fills at rate per second, holds at most one second's worth
// (burst), and every read spends as many tokens as it read; otherwise it waits.
//
// Why global: the user asks for "2 MB/s in total", not "2 MB/s per file".
// With a limit of 0 nothing waits and the cost is taking a single lock.
//
// Changing the rate while running is safe: the next read sees the new value.
type Throttle struct {
	mu     sync.Mutex
	rate   int64 // bytes/s; 0 = unlimited
	tokens float64
	last   time.Time
}

// SetRate sets the limit in bytes per second; 0 removes it.
func (t *Throttle) SetRate(bytesPerSec int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if bytesPerSec < 0 {
		bytesPerSec = 0
	}
	t.rate = bytesPerSec
	// Old accumulation must not overflow under the new limit.
	if t.tokens > float64(bytesPerSec) {
		t.tokens = float64(bytesPerSec)
	}
	t.last = time.Now()
}

// Rate returns the current limit.
func (t *Throttle) Rate() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rate
}

// Wait waits until n bytes are allowed. Returns immediately without a limit.
//
// If there aren't enough tokens the request is taken anyway and the bucket
// goes into DEBT (below zero); the caller waits until the debt is paid off.
// The cap only bounds accumulation, not the request itself: otherwise, when
// the limit is smaller than the read chunk (8 KB/s limit, 16 KB chunk), the
// bucket would never fill enough and the download would stall forever. Later
// callers see the debt and wait their turn, so the total rate still stays at
// the limit.
func (t *Throttle) Wait(ctx context.Context, n int) error {
	if t == nil || n <= 0 {
		return nil
	}
	t.mu.Lock()
	if t.rate <= 0 {
		t.mu.Unlock()
		return nil
	}
	now := time.Now()
	if !t.last.IsZero() {
		t.tokens += now.Sub(t.last).Seconds() * float64(t.rate)
	}
	t.last = now
	if cap := float64(t.rate); t.tokens > cap {
		t.tokens = cap
	}
	t.tokens -= float64(n)
	if t.tokens >= 0 {
		t.mu.Unlock()
		return nil
	}
	wait := time.Duration(-t.tokens / float64(t.rate) * float64(time.Second))
	t.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// chunkFor shrinks the read chunk while a limit is set: reading 256 KB at a
// 100 KB/s limit means 2.5-second waits and a progress bar that looks stuck.
// The chunk drops to a quarter second's worth of data (at least 16 KB).
func (t *Throttle) chunkFor(bufLen int) int {
	if t == nil {
		return bufLen
	}
	t.mu.Lock()
	rate := t.rate
	t.mu.Unlock()
	if rate <= 0 {
		return bufLen
	}
	quarter := int(rate / 4)
	if quarter < 16<<10 {
		quarter = 16 << 10
	}
	if quarter < bufLen {
		return quarter
	}
	return bufLen
}
