// Package procstat reads a process's start time from /proc, which tells a
// process apart from a later one that reuses its PID.
package procstat

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// StartTime returns a live process's start time in clock ticks after boot. A
// process that is gone, or a zombie, which has exited, is reported as not alive.
func StartTime(pid int) (uint64, bool, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	// The command name may contain spaces and parentheses; fields follow the
	// last ')'. fields[0] is the state (field 3); the start time is field 22.
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return 0, false, errors.New("unexpected /proc stat format")
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 {
		return 0, false, errors.New("unexpected /proc stat format")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return 0, false, nil
	}
	started, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse process start time: %w", err)
	}
	return started, true, nil
}
