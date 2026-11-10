package spool

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"time"
)

// processStart returns an identity for the process currently holding pid
// that a later process reusing the pid cannot share (the boot ID plus its
// start time in clock ticks since boot), plus its approximate wall-clock
// start time. ok is false when the process cannot be inspected.
func processStart(pid int) (id string, at time.Time, ok bool) {
	if pid <= 0 {
		return "", time.Time{}, false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", time.Time{}, false
	}
	// The command name (field 2) may contain spaces and parentheses; the
	// fields after its closing parenthesis start at field 3, so the start
	// time (field 22) is the twentieth of them.
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return "", time.Time{}, false
	}
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 20 {
		return "", time.Time{}, false
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	// Without the boot ID the identity would not be comparable with one
	// recorded earlier, so the process counts as uninspectable.
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", time.Time{}, false
	}
	id = "linux:" + strings.TrimSpace(string(boot)) + ":" + fields[19]
	// USER_HZ is 100 on every Linux ABI Go supports.
	if btime, ok := bootTime(); ok {
		at = btime.Add(time.Duration(ticks) * (time.Second / 100))
	}
	return id, at, true
}

// bootTime reads the boot time ("btime") from /proc/stat.
func bootTime() (time.Time, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, found := strings.CutPrefix(line, "btime "); found {
			sec, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				return time.Time{}, false
			}
			return time.Unix(sec, 0), true
		}
	}
	return time.Time{}, false
}
