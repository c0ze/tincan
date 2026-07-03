//go:build !windows

package spool

// POSIX renames don't hold Windows-style sharing locks; nothing is transient.
func isTransientShare(error) bool { return false }
