package db

import (
	"strings"
	"testing"
	"time"
)

func TestRecordHostCapabilityObservationRoundTripAndUpsert(t *testing.T) {
	database := SetupTestDB(t)
	t1 := time.Unix(1_787_700_000, 0)
	t2 := t1.Add(time.Hour)

	for _, observation := range []HostCapabilityObservation{
		{
			Host:       "studio",
			Label:      " Agent:Claude ",
			Source:     "probe:agent-review",
			Observed:   false,
			Detail:     "loggedIn=false",
			ObservedAt: t1,
		},
		{
			Host:       "studio",
			Label:      "agent:claude",
			Source:     "probe:manual-check",
			Observed:   true,
			Detail:     "binary and credentials usable",
			ObservedAt: t1,
		},
		{
			Host:       "cool30",
			Label:      "agent:codex",
			Source:     "probe:agent-review",
			Observed:   true,
			ObservedAt: t1,
		},
	} {
		if err := RecordHostCapabilityObservation(database, observation); err != nil {
			t.Fatalf("RecordHostCapabilityObservation(%+v): %v", observation, err)
		}
	}

	if err := RecordHostCapabilityObservation(database, HostCapabilityObservation{
		Host:       "studio",
		Label:      "agent:claude",
		Source:     "probe:agent-review",
		Observed:   true,
		Detail:     "credentials repaired",
		ObservedAt: t2,
	}); err != nil {
		t.Fatalf("replace observation: %v", err)
	}

	studio, err := ListHostCapabilityObservations(database, "studio")
	if err != nil {
		t.Fatalf("ListHostCapabilityObservations: %v", err)
	}
	if len(studio) != 2 {
		t.Fatalf("studio observations = %+v, want two observer rows", studio)
	}
	if got := studio[0]; got.Label != "agent:claude" || got.Source != "probe:agent-review" || !got.Observed || got.Detail != "credentials repaired" || !got.ObservedAt.Equal(t2) {
		t.Errorf("updated observation = %+v", got)
	}
	if got := studio[1]; got.Source != "probe:manual-check" || !got.ObservedAt.Equal(t1) {
		t.Errorf("independent observer row = %+v", got)
	}

	all, err := ListAllHostCapabilityObservations(database)
	if err != nil {
		t.Fatalf("ListAllHostCapabilityObservations: %v", err)
	}
	if len(all) != 2 || len(all["cool30"]) != 1 || len(all["studio"]) != 2 {
		t.Fatalf("all observations = %+v", all)
	}
}

func TestRecordHostCapabilityObservationRejectsInvalidSources(t *testing.T) {
	database := SetupTestDB(t)
	base := HostCapabilityObservation{
		Host:       "studio",
		Label:      "agent:codex",
		Observed:   true,
		ObservedAt: time.Unix(1_787_700_000, 0),
	}

	for _, source := range []string{
		"",
		"operator",
		"probe:",
		"probe:AgentReview",
		"probe:-agent-review",
		"probe:" + strings.Repeat("a", 65),
	} {
		t.Run(source, func(t *testing.T) {
			observation := base
			observation.Source = source
			if err := RecordHostCapabilityObservation(database, observation); err == nil {
				t.Fatalf("source %q was accepted", source)
			}
		})
	}
}

func TestRecordHostCapabilityObservationAcceptsFullSourceShape(t *testing.T) {
	database := SetupTestDB(t)
	for _, source := range []string{"probe:a", "probe:agent-review_1.2"} {
		observation := HostCapabilityObservation{
			Host:       "studio",
			Label:      "agent:codex",
			Source:     source,
			Observed:   true,
			ObservedAt: time.Unix(1_787_700_000, 0),
		}
		if err := RecordHostCapabilityObservation(database, observation); err != nil {
			t.Errorf("source %q: %v", source, err)
		}
	}
}

// TestHostCapabilityObservationDetailCapStaysSmall pins the value, not just the
// comparison. The cap is what keeps detail provisional — small enough to hold a
// short note and too small to carry a payload format someone could come to
// depend on before the field is closed into a vocabulary. Widening it is a
// design change, so it should cost a failing test rather than pass silently.
// The neighbouring cap test derives its inputs from the constant and so cannot
// catch that on its own.
func TestHostCapabilityObservationDetailCapStaysSmall(t *testing.T) {
	if hostCapabilityObservationDetailMaxBytes != 256 {
		t.Errorf("detail cap = %d, want 256 — see HostCapabilityObservation in specs/inventory-placement.allium before changing it",
			hostCapabilityObservationDetailMaxBytes)
	}
}

func TestRecordHostCapabilityObservationCapsDetailForProvisionalField(t *testing.T) {
	database := SetupTestDB(t)
	base := HostCapabilityObservation{
		Host:       "studio",
		Label:      "agent:codex",
		Source:     "probe:agent-review",
		Observed:   true,
		ObservedAt: time.Unix(1_787_700_000, 0),
	}

	atLimit := base
	atLimit.Detail = strings.Repeat("a", hostCapabilityObservationDetailMaxBytes)
	if err := RecordHostCapabilityObservation(database, atLimit); err != nil {
		t.Fatalf("detail at limit was rejected: %v", err)
	}

	overLimit := base
	overLimit.Detail = strings.Repeat("a", hostCapabilityObservationDetailMaxBytes+1)
	if err := RecordHostCapabilityObservation(database, overLimit); err == nil {
		t.Fatal("detail over limit was accepted")
	}
}

func TestRecordHostCapabilityObservationDoesNotRegisterExecutionTarget(t *testing.T) {
	database := SetupTestDB(t)
	if err := RecordHostCapabilityObservation(database, HostCapabilityObservation{
		Host:       "observation-only-host",
		Label:      "agent:codex",
		Source:     "probe:agent-review",
		Observed:   true,
		ObservedAt: time.Unix(1_787_700_000, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM execution_targets WHERE host = ?`, "observation-only-host").Scan(&count); err != nil {
		t.Fatalf("count execution targets: %v", err)
	}
	if count != 0 {
		t.Fatalf("execution target count = %d, want 0", count)
	}
}
