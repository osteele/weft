package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestBuildProviderStartupDiagnosisDirectInstance(t *testing.T) {
	database := db.SetupTestDB(t)
	inst := &cloud.Instance{
		ProviderID:     "pod-123",
		Provider:       cloud.ProviderRunpod,
		Status:         cloud.ProviderStatusCreating,
		IntendedStatus: "running",
		DataCenter:     "US-KS-2",
		MachineID:      "machine-9",
	}

	report := buildProviderStartupDiagnosis(database, cloud.ProviderRunpod, "pod-123", inst, nil, "", 6*time.Minute)
	if report.ProviderStatus != cloud.ProviderStatusCreating {
		t.Fatalf("ProviderStatus = %q, want creating", report.ProviderStatus)
	}
	if report.DataCenter != "US-KS-2" {
		t.Fatalf("DataCenter = %q, want US-KS-2", report.DataCenter)
	}
	if report.Bootstrap.TerminateAfterSeconds == 0 {
		t.Fatal("bootstrap terminate threshold should be populated")
	}
	if report.FirstRegistration.Scope != "global" {
		t.Fatalf("FirstRegistration.Scope = %q, want global fallback without samples", report.FirstRegistration.Scope)
	}
	if report.Recommendation == "" {
		t.Fatal("Recommendation should be populated")
	}

	out := formatProviderStartupDiagnosis(report)
	for _, want := range []string{
		"Provider instance runpod:pod-123",
		"Status:     creating",
		"Location:   US-KS-2",
		"Historical Startup:",
		"Bootstrap:",
		"First registration:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("formatted output missing %q:\n%s", want, out)
		}
	}
}

func TestBuildProviderStartupDiagnosisNotesLookupFailure(t *testing.T) {
	database := db.SetupTestDB(t)
	report := buildProviderStartupDiagnosis(database, cloud.ProviderVastai, "44124168", nil, errProviderLookupTest{}, "", 0)
	if report.ProviderError == "" {
		t.Fatal("ProviderError should be populated")
	}
	if len(report.Notes) < 2 {
		t.Fatalf("Notes = %v, want lookup and elapsed/data-center notes", report.Notes)
	}
}

type errProviderLookupTest struct{}

func (errProviderLookupTest) Error() string { return "provider unavailable" }
