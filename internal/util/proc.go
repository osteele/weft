package util

import (
	"os"
	"syscall"
)

// IsProcessAlive reports whether a process with the given PID is reachable.
// Uses signal 0, which doesn't deliver but tells us whether the process
// exists and is signalable from this process. Returns false for non-positive
// PIDs and for PIDs that don't resolve to a running, signalable process.
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
