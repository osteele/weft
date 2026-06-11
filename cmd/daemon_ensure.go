package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/osteele/weft/internal/daemoncontrol"
)

var ensureDaemonStartedFunc = ensureDaemonStarted

func ensureDaemonStarted(paths daemoncontrol.Paths, wait time.Duration) (daemoncontrol.Status, bool, error) {
	status, err := daemoncontrol.CurrentStatus(paths)
	if err != nil {
		return status, false, err
	}
	if status.Live {
		return status, false, nil
	}
	if status.Installed {
		if err := daemoncontrol.Load(paths); err != nil {
			return status, false, err
		}
	} else {
		if _, err := daemoncontrol.StartDetached(paths); err != nil {
			return status, false, err
		}
	}
	deadline := time.Now().Add(wait)
	for {
		status, err = daemoncontrol.CurrentStatus(paths)
		if err != nil {
			return status, true, err
		}
		if status.Live {
			return status, true, nil
		}
		if time.Now().After(deadline) {
			return status, true, fmt.Errorf("daemon did not report running within %s", wait)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func ensureDaemonForWork(w io.Writer) {
	paths := daemoncontrol.DefaultPaths()
	status, started, err := ensureDaemonStartedFunc(paths, 2*time.Second)
	if err != nil {
		fmt.Fprintf(w, "warning: daemon is not running and could not be started: %v\n", err)
		fmt.Fprintf(w, "         queued work may not dispatch until `weft daemon start` succeeds\n")
		return
	}
	if started {
		fmt.Fprintf(w, "Started daemon (PID %d)\n", status.PID)
	}
}
