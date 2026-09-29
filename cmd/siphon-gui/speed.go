package main

import (
	"fmt"
	"sync"
	"time"
)

// speedo produces a bytes-per-second estimate.
//
// Why not a plain division: an instant measurement over 250 ms windows jumps
// around a lot (the network fluctuates, disk buffers drain) and the user sees
// a number that keeps changing. A plain average since the start of the run,
// on the other hand, can't show slowdowns; even if the speed halves, the
// average stays high for minutes.
//
// An exponential moving average sits between the two: it weights the last
// measurement but isn't thrown off by a single spike.
type speedo struct {
	mu        sync.Mutex
	last      time.Time
	lastBytes int64
	ema       float64
	started   bool
}

// speedAlpha is the weight of the last measurement. 0.3 is a readable
// balance in practice: it shows a slowdown within a few seconds without
// making the number flicker.
const speedAlpha = 0.3

// update processes a new cumulative byte value and returns the current speed.
func (s *speedo) update(bytes int64, now time.Time) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		// No speed can be derived from the first sample: no time has passed.
		s.started, s.last, s.lastBytes = true, now, bytes
		return 0
	}

	dt := now.Sub(s.last).Seconds()
	if dt <= 0 {
		return s.ema
	}
	db := bytes - s.lastBytes
	if db < 0 {
		// The cumulative counter went backwards: the download restarted (the
		// server returned 200, the .part was reset). Continue from zero
		// instead of showing a negative speed.
		db = 0
	}
	inst := float64(db) / dt
	if s.ema == 0 {
		s.ema = inst
	} else {
		s.ema = speedAlpha*inst + (1-speedAlpha)*s.ema
	}
	s.last, s.lastBytes = now, bytes
	return s.ema
}

// rate returns the last computed speed.
func (s *speedo) rate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ema
}

// humanRate formats a speed for humans. If the speed is unknown it returns
// an empty string: "0 B/s" tells the user "it stopped", while there's simply
// no measurement yet.
func humanRate(bps float64) string {
	if bps <= 0 {
		return ""
	}
	return humanBytes(int64(bps)) + "/s"
}

// humanETA formats the remaining time for humans.
// If the speed or the remainder is unknown it returns empty; we don't show
// made-up estimates.
func humanETA(remaining int64, bps float64) string {
	if remaining <= 0 || bps <= 0 {
		return ""
	}
	secs := float64(remaining) / bps
	if secs > 24*3600 {
		// Estimates longer than a day are meaningless in practice; giving a
		// number would be misleading.
		return ""
	}
	d := time.Duration(secs) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
