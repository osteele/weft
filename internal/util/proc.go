package util

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// IsProcessAlive reports whether a process with the given PID is reachable.
// Uses signal 0, which doesn't deliver but tells us whether the process
// exists and is signalable from this process. Zombie processes are treated as
// not alive: they still have a process table entry, but cannot make progress or
// release resources they owned before exit.
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if proc.Signal(syscall.Signal(0)) != nil {
		return false
	}
	return !processIsZombie(pid)
}

func processIsZombie(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
