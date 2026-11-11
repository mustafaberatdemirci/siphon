package net

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A download that had been coming in for longer than MaxElapsed was given up
// at its first dropped connection: the time ran from the first attempt's
// start, working time included. An attempt that got further starts the
// budget over.
func TestPolicyLongAttemptThatProgressedIsRetried(t *testing.T) {
	calls := 0
	p := Policy{
		MaxAttempts: 5,
		MaxElapsed:  50 * time.Millisecond,
		Backoff:     Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond},
	}
	err := p.Do(context.Background(), func(int) error {
		calls++
		if calls == 1 {
			time.Sleep(120 * time.Millisecond) // working, for longer than the budget
			return Progressed(&retryableTestErr{})
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("calls = %d, err = %v; want a second attempt that succeeds", calls, err)
	}
}

// Progress doesn't spend attempts either: a file that keeps coming may drop
// its connection more often than MaxAttempts.
func TestPolicyProgressDoesNotSpendAttempts(t *testing.T) {
	calls := 0
	err := fastPolicy(3).Do(context.Background(), func(int) error {
		calls++
		if calls <= 10 {
			return Progressed(&retryableTestErr{n: calls})
		}
		return nil
	})
	if err != nil || calls != 11 {
		t.Fatalf("calls = %d, err = %v; want 11 attempts ending in success", calls, err)
	}
}

// Without progress the budget is what it was, counted from the last attempt
// that got further, and the message says how many attempts were really made.
func TestPolicyCountsFailuresSinceLastProgress(t *testing.T) {
	calls := 0
	err := fastPolicy(3).Do(context.Background(), func(int) error {
		calls++
		if calls == 2 {
			return Progressed(&retryableTestErr{n: calls})
		}
		return &retryableTestErr{n: calls}
	})
	// 1 fails, 2 progresses (the count starts over), 3, 4 and 5 fail.
	if !errors.Is(err, ErrExhausted) || calls != 5 {
		t.Fatalf("calls = %d, err = %v; want ErrExhausted after 5", calls, err)
	}
	if !strings.Contains(err.Error(), "(5 attempts)") {
		t.Errorf("the message doesn't say the attempts made: %v", err)
	}
}

// The message used to say the configured number whatever happened.
func TestPolicyMessageCountsAttemptsMade(t *testing.T) {
	calls := 0
	p := Policy{
		MaxAttempts: 5,
		MaxElapsed:  30 * time.Millisecond,
		Backoff:     Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond},
	}
	err := p.Do(context.Background(), func(int) error {
		calls++
		time.Sleep(60 * time.Millisecond) // a slow error: the time runs out first
		return &retryableTestErr{}
	})
	if !errors.Is(err, ErrExhausted) || calls != 1 {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
	if !strings.Contains(err.Error(), "(1 attempts)") {
		t.Errorf("the message doesn't say the attempts made: %v", err)
	}
}

// A permanent error stays permanent, progress or not.
func TestPolicyProgressedPermanentErrorIsNotRetried(t *testing.T) {
	calls := 0
	perm := errors.New("HTTP 404")
	err := fastPolicy(5).Do(context.Background(), func(int) error {
		calls++
		return Progressed(perm)
	})
	if !errors.Is(err, perm) || calls != 1 {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
}
