package main

import "testing"

// Work handed to the UI thread after the app started quitting isn't done.
func TestNothingIsDrawnOnceQuitting(t *testing.T) {
	q, _, _ := newTestQueue(t, "a.bin")
	ran := 0
	q.do(func() { ran++ })
	if ran != 1 {
		t.Fatalf("before quitting the work ran %d times, want once", ran)
	}
	q.close()
	q.do(func() { ran++ })
	q.close() // twice is fine
	if ran != 1 {
		t.Errorf("work ran after the app started quitting")
	}
}
