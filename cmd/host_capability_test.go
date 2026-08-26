package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

func TestHostCapabilityObserveAcceptsHostAndLabel(t *testing.T) {
	if err := hostCapabilityObserveCmd.Args(hostCapabilityObserveCmd, []string{"studio", "agent:codex"}); err != nil {
		t.Fatalf("Args returned error: %v", err)
	}
}

func TestHostCapabilityObserveRecordsAdvisoryFinding(t *testing.T) {
	database := db.SetupTestDB(t)
	hostCapabilityObserveSource = "probe:agent-review"
	hostCapabilityObserveAbsent = true
	hostCapabilityObserveDetail = "loggedIn=false"
	t.Cleanup(func() {
		hostCapabilityObserveSource = ""
		hostCapabilityObserveAbsent = false
		hostCapabilityObserveDetail = ""
	})

	var stderr bytes.Buffer
	command := &cobra.Command{}
	command.SetErr(&stderr)
	if err := runHostCapabilityObserve(command, []string{"studio", "Agent:Claude"}); err != nil {
		t.Fatalf("runHostCapabilityObserve: %v", err)
	}

	observations, err := db.ListHostCapabilityObservations(database, "studio")
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].Label != "agent:claude" || observations[0].Observed || observations[0].Detail != "loggedIn=false" {
		t.Fatalf("observations = %+v", observations)
	}
	if message := stderr.String(); !strings.Contains(message, "does not affect placement") || !strings.Contains(message, "absent") {
		t.Fatalf("confirmation = %q", message)
	}
}

func TestHostCapabilityObserveRejectsInstanceIDBeforeOpeningDatabase(t *testing.T) {
	db.SetupTestDB(t)
	command := &cobra.Command{}
	err := runHostCapabilityObserve(command, []string{"wi123", "agent:codex"})
	if err == nil || !strings.Contains(err.Error(), "cloud instances are ephemeral") {
		t.Fatalf("error = %v", err)
	}
}
