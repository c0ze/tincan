//go:build windows

package spool

import (
	"errors"
	"syscall"
)

// Windows errnos produced when a racing receiver's in-flight handle briefly
// locks a claim file: ERROR_SHARING_VIOLATION (32) and ERROR_LOCK_VIOLATION (33).
func isTransientShare(err error) bool {
	return errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(33))
}
