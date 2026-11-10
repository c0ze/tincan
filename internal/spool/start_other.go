//go:build !darwin && !linux && !windows

package spool

import "time"

// processStart is unavailable here: liveness falls back to the PID check.
func processStart(int) (string, time.Time, bool) { return "", time.Time{}, false }
