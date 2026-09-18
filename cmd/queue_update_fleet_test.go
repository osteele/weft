package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/inventory"
	"github.com/spf13/cobra"
)

func resetQueueUpdateFlags(t *testing.T) {
	t.Helper()
	originalAll, originalHosts := queueUpdateAll, queueUpdateHosts
	t.Cleanup(func() { queueUpdateAll, queueUpdateHosts = originalAll, originalHosts })
	queueUpdateAll, queueUpdateHosts = false, nil
}

func TestResolveQueueUpdateHostsRejectsConflictingSelectors(t *testing.T) {
	inventory.UseTestHosts(t)
	cmd := &cobra.Command{}

	resetQueueUpdateFlags(t)
	queueUpdateAll = true
	queueUpdateHosts = []string{"host-alpha"}
	if _, err := resolveQueueUpdateHosts(cmd, nil); err == nil {
		t.Error("expected --all with --hosts to be rejected")
	}

	resetQueueUpdateFlags(t)
	queueUpdateAll = true
	if _, err := resolveQueueUpdateHosts(cmd, []string{"host-alpha"}); err == nil {
		t.Error("expected a positional host with --all to be rejected")
	}
}

// TestResolveQueueUpdateHostsRejectsHostFlagWithFleetSelector covers the
// --host form of the same conflict the positional argument already raised:
// without this, a fleet selector converges the whole inventory while the
// explicitly named host is discarded without a word.
func TestResolveQueueUpdateHostsRejectsHostFlagWithFleetSelector(t *testing.T) {
	inventory.UseTestHosts(t)
	originalHost := queueHost
	t.Cleanup(func() { queueHost = originalHost })

	for _, tc := range []struct {
		name  string
		all   bool
		hosts []string
	}{
		{"all", true, nil},
		{"hosts", false, []string{"host-beta"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetQueueUpdateFlags(t)
			queueUpdateAll, queueUpdateHosts = tc.all, tc.hosts
			queueHost = "host-alpha"
			if _, err := resolveQueueUpdateHosts(&cobra.Command{}, nil); err == nil {
				t.Errorf("expected --host with %s to be rejected", tc.name)
			}
		})
	}
}

func TestResolveQueueUpdateHostsAllLoadsInventory(t *testing.T) {
	inventory.UseTestHosts(t)
	resetQueueUpdateFlags(t)
	queueUpdateAll = true

	hosts, err := resolveQueueUpdateHosts(&cobra.Command{}, nil)
	if err != nil {
		t.Fatalf("resolveQueueUpdateHosts: %v", err)
	}
	if len(hosts) < 2 {
		t.Fatalf("expected --all to select the whole inventory, got %v", hosts)
	}
}

func TestResolveQueueUpdateHostsRejectsUnknownHost(t *testing.T) {
	inventory.UseTestHosts(t)
	resetQueueUpdateFlags(t)
	queueUpdateHosts = []string{"host-alpha", "not-a-real-host"}

	_, err := resolveQueueUpdateHosts(&cobra.Command{}, nil)
	if err == nil {
		t.Fatal("expected an unknown host to be rejected before any deployment runs")
	}
	if !strings.Contains(err.Error(), "not-a-real-host") {
		t.Errorf("error should name the offending host, got: %v", err)
	}
}

// TestReportQueueUpdateOutcomesSeparatesUnreachable is the substantive
// property: a host that could not be reached must not be reported as a
// convergence failure, because not having looked is not the same as having
// looked and found the host wrong.
func TestReportQueueUpdateOutcomesSeparatesUnreachable(t *testing.T) {
	var out bytes.Buffer
	err := reportQueueUpdateOutcomes(&out, []queueUpdateOutcome{
		{host: "host-alpha"},
		{host: "host-beta", err: errors.New("ssh: connect to host host-beta port 22: Operation timed out"), unreachable: true},
		{host: "host-gamma", err: errors.New("deploy agent on host-gamma: build failed")},
	})
	if err == nil {
		t.Fatal("expected a non-nil error when some hosts did not converge")
	}
	text := out.String()
	if !strings.Contains(text, "host-alpha       converged") && !strings.Contains(text, "host-alpha   converged") {
		t.Errorf("converged host missing from summary:\n%s", text)
	}
	if !strings.Contains(text, "unreachable") || !strings.Contains(text, "remains unobserved") {
		t.Errorf("unreachable host not reported as unobserved:\n%s", text)
	}
	if !strings.Contains(text, "host-gamma") || !strings.Contains(text, "failed:") {
		t.Errorf("failed host not reported as a failure:\n%s", text)
	}
	if strings.Contains(err.Error(), "1 failed, 0 unreachable") {
		t.Errorf("unreachable host was counted as a failure: %v", err)
	}
	if !strings.Contains(err.Error(), "1 failed, 1 unreachable") {
		t.Errorf("summary error should count failure and unreachability separately, got: %v", err)
	}
}

func TestReportQueueUpdateOutcomesAllConvergedReturnsNil(t *testing.T) {
	var out bytes.Buffer
	err := reportQueueUpdateOutcomes(&out, []queueUpdateOutcome{
		{host: "host-alpha"},
		{host: "host-beta"},
	})
	if err != nil {
		t.Fatalf("expected nil error when every host converged, got %v", err)
	}
	if strings.Contains(out.String(), "failed") || strings.Contains(out.String(), "unreachable") {
		t.Errorf("clean run should not mention failure:\n%s", out.String())
	}
}
