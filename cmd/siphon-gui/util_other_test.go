//go:build !windows

package main

import "testing"

// Outside Windows there are no drive letters; what normalizeDir still does is
// strip quotes, spaces and the trailing separator.
func TestNormalizeDirUnix(t *testing.T) {
	for in, want := range map[string]string{
		"/home/me/Downloads/":       "/home/me/Downloads",
		`  "/home/me/sub folder"  `: "/home/me/sub folder",
		"/":                         "/",
		"":                          "",
		`""`:                        "",
	} {
		if got := normalizeDir(in); got != want {
			t.Errorf("normalizeDir(%q) = %q, want %q", in, got, want)
		}
	}
}
