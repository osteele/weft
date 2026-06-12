package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/daemoncontrol"
)

func TestEnsureDaemonForWorkReportsStarted(t *testing.T) {
	oldEnsure := ensureDaemonStartedFunc
	ensureDaemonStartedFunc = func(paths daemoncontrol.Paths, wait time.Duration) (daemoncontrol.Status, daemoncontrol.EnsureAction, error) {
		return daemoncontrol.Status{PID: 1234, Live: true}, daemoncontrol.EnsureStarted, nil
	}
	t.Cleanup(func() { ensureDaemonStartedFunc = oldEnsure })

	var out bytes.Buffer
	ensureDaemonForWork(&out)
	if got := out.String(); !strings.Contains(got, "Started daemon (PID 1234)") {
		t.Fatalf("output = %q, want started message", got)
	}
}

func TestEnsureDaemonForWorkReportsRestarted(t *testing.T) {
	oldEnsure := ensureDaemonStartedFunc
	ensureDaemonStartedFunc = func(paths daemoncontrol.Paths, wait time.Duration) (daemoncontrol.Status, daemoncontrol.EnsureAction, error) {
		return daemoncontrol.Status{PID: 5678, Live: true}, daemoncontrol.EnsureRestarted, nil
	}
	t.Cleanup(func() { ensureDaemonStartedFunc = oldEnsure })

	var out bytes.Buffer
	ensureDaemonForWork(&out)
	if got := out.String(); !strings.Contains(got, "Restarted daemon (PID 5678)") {
		t.Fatalf("output = %q, want restarted message", got)
	}
}

func TestEnsureDaemonForWorkWarnsWithoutFailing(t *testing.T) {
	oldEnsure := ensureDaemonStartedFunc
	ensureDaemonStartedFunc = func(paths daemoncontrol.Paths, wait time.Duration) (daemoncontrol.Status, daemoncontrol.EnsureAction, error) {
		return daemoncontrol.Status{}, daemoncontrol.EnsureNoop, errors.New("boom")
	}
	t.Cleanup(func() { ensureDaemonStartedFunc = oldEnsure })

	var out bytes.Buffer
	ensureDaemonForWork(&out)
	got := out.String()
	if !strings.Contains(got, "warning: daemon is not current") || !strings.Contains(got, "boom") {
		t.Fatalf("output = %q, want warning with error", got)
	}
}
