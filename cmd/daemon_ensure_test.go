package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/daemoncontrol"
)

func TestEnsureDaemonForWorkDistinguishesProbeAndLifecycleFailures(t *testing.T) {
	cause := errors.New("test failure")
	for _, tt := range []struct {
		name    string
		action  daemoncontrol.EnsureAction
		err     error
		unknown bool
	}{
		{
			name:    "identity unknown",
			action:  daemoncontrol.EnsureNoop,
			err:     fmt.Errorf("ensure: %w", &daemoncontrol.IdentityProbeError{Err: cause}),
			unknown: true,
		},
		{
			name:   "start failed before action recorded",
			action: daemoncontrol.EnsureNoop,
			err:    cause,
		},
		{
			name:   "restart failed",
			action: daemoncontrol.EnsureRestarted,
			err:    cause,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			oldEnsure := ensureDaemonStartedFunc
			ensureDaemonStartedFunc = func(daemoncontrol.Paths, time.Duration) (daemoncontrol.Status, daemoncontrol.EnsureAction, error) {
				return daemoncontrol.Status{}, tt.action, tt.err
			}
			t.Cleanup(func() { ensureDaemonStartedFunc = oldEnsure })

			var out bytes.Buffer
			ensureDaemonForWork(&out)
			got := out.String()
			if !strings.Contains(got, cause.Error()) {
				t.Fatalf("diagnostic lost failure cause: %q", got)
			}
			if tt.unknown {
				if !strings.Contains(got, "unknown") || !strings.Contains(got, "no lifecycle action") {
					t.Fatalf("probe failure must report uncertainty and no action: %q", got)
				}
				for _, misleading := range []string{"weft daemon restart", "not current", "could not be started", "stale", "dead"} {
					if strings.Contains(got, misleading) {
						t.Errorf("probe failure gives unjustified lifecycle diagnosis %q: %q", misleading, got)
					}
				}
			} else {
				if strings.Contains(got, "unknown") || strings.Contains(got, "no lifecycle action") {
					t.Fatalf("lifecycle failure misreported as an observation failure: %q", got)
				}
				if !strings.Contains(got, "weft daemon restart") {
					t.Fatalf("lifecycle failure lost recovery advice: %q", got)
				}
			}
		})
	}
}
