//go:build !windows

package fsx

// transient: elsewhere an open file doesn't stop a rename.
func transient(error) bool { return false }
