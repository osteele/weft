package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/osteele/weft/internal/daemoncontrol"
)

var ensureDaemonStartedFunc = ensureDaemonStarted

func ensureDaemonStarted(paths daemoncontrol.Paths, wait time.Duration) (daemoncontrol.Status, daemoncontrol.EnsureAction, error) {
	return daemoncontrol.EnsureCurrent(paths, wait)
}

func ensureDaemonForWork(w io.Writer) {
	paths := daemoncontrol.DefaultPaths()
	status, action, err := ensureDaemonStartedFunc(paths, 2*time.Second)
	if err != nil {
		fmt.Fprintf(w, "warning: daemon is not current and could not be started or restarted: %v\n", err)
		fmt.Fprintf(w, "         queued work may not dispatch until `weft daemon restart` succeeds\n")
		return
	}
	switch action {
	case daemoncontrol.EnsureStarted:
		fmt.Fprintf(w, "Started daemon (PID %d)\n", status.PID)
	case daemoncontrol.EnsureRestarted:
		fmt.Fprintf(w, "Restarted daemon (PID %d)\n", status.PID)
	}
}
