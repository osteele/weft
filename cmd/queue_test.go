package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestBuildQueueEditDependencies(t *testing.T) {
	database := db.SetupTestDB(t)

	targetID, err := db.RecordQueued(database, "hostA", "/tmp", "echo target", "target")
	if err != nil {
		t.Fatalf("record target job: %v", err)
	}
	successID, err := db.RecordQueued(database, "hostA", "/tmp", "echo success", "success")
	if err != nil {
		t.Fatalf("record success job: %v", err)
	}
	anyID, err := db.RecordQueued(database, "hostA", "/tmp", "echo any", "any")
	if err != nil {
		t.Fatalf("record completion job: %v", err)
	}

	deps, err := buildQueueEditDependencies(database, "hostA", targetID,
		[]string{fmt.Sprintf("%d", successID)},
		[]string{fmt.Sprintf("%d", anyID)},
	)
	if err != nil {
		t.Fatalf("build dependencies: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps, got %d", len(deps))
	}
	if deps[0].JobID != successID || deps[0].AllowFailure {
		t.Fatalf("expected success dep to require success, got %+v", deps[0])
	}
	if deps[1].JobID != anyID || !deps[1].AllowFailure {
		t.Fatalf("expected completion dep to allow failure, got %+v", deps[1])
	}

	// Self-dependency should fail
	if _, err := buildQueueEditDependencies(database, "hostA", targetID, []string{fmt.Sprintf("%d", targetID)}, nil); err == nil {
		t.Fatal("expected self-dependency to error")
	}

	// Cross-host dependency should fail
	otherHostID, err := db.RecordQueued(database, "hostB", "/tmp", "echo other", "other")
	if err != nil {
		t.Fatalf("record other host job: %v", err)
	}
	_, err = buildQueueEditDependencies(database, "hostA", targetID, []string{fmt.Sprintf("%d", otherHostID)}, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot depend") {
		t.Fatalf("expected host mismatch error, got %v", err)
	}
}

func TestBuildQueueEditDependenciesModes(t *testing.T) {
	database := db.SetupTestDB(t)
	targetID, err := db.RecordQueued(database, "hostA", "/tmp", "echo target", "target")
	if err != nil {
		t.Fatalf("record target job: %v", err)
	}
	jobA, err := db.RecordQueued(database, "hostA", "/tmp", "echo A", "A")
	if err != nil {
		t.Fatalf("record job A: %v", err)
	}
	jobB, err := db.RecordQueued(database, "hostA", "/tmp", "echo B", "B")
	if err != nil {
		t.Fatalf("record job B: %v", err)
	}

	deps, err := buildQueueEditDependencies(database, "hostA", targetID,
		[]string{fmt.Sprintf("%d:any,%d:success", jobA, jobB)},
		nil,
	)
	if err != nil {
		t.Fatalf("build dependencies: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps, got %d", len(deps))
	}
	if deps[0].JobID != jobA || !deps[0].AllowFailure {
		t.Fatalf("expected job A to allow failure, got %+v", deps[0])
	}
	if deps[1].JobID != jobB || deps[1].AllowFailure {
		t.Fatalf("expected job B to require success, got %+v", deps[1])
	}
}

func TestBuildQueueEditDependenciesValidation(t *testing.T) {
	database := db.SetupTestDB(t)
	targetID, err := db.RecordQueued(database, "hostA", "/tmp", "echo target", "target")
	if err != nil {
		t.Fatalf("record target job: %v", err)
	}
	jobA, err := db.RecordQueued(database, "hostA", "/tmp", "echo A", "A")
	if err != nil {
		t.Fatalf("record job A: %v", err)
	}

	// '+' suffix on first entry should set allow-failure
	deps, err := buildQueueEditDependencies(database, "hostA", targetID, []string{fmt.Sprintf("%d+", jobA)}, nil)
	if err != nil {
		t.Fatalf("plus suffix: %v", err)
	}
	if len(deps) != 1 || !deps[0].AllowFailure {
		t.Fatalf("expected allow failure from suffix, got %+v", deps)
	}

	// Duplicated entries keep the first interpretation
	deps, err = buildQueueEditDependencies(database, "hostA", targetID, []string{fmt.Sprintf("%d,%d+", jobA, jobA)}, nil)
	if err != nil {
		t.Fatalf("dedupe deps: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep after dedupe, got %d", len(deps))
	}
	if deps[0].AllowFailure {
		t.Fatalf("expected first entry to win when deduping")
	}

	// Unknown mode
	_, err = buildQueueEditDependencies(database, "hostA", targetID, []string{fmt.Sprintf("%d:bogus", jobA)}, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown dependency mode") {
		t.Fatalf("expected mode error, got %v", err)
	}

	// Non-numeric ID
	_, err = buildQueueEditDependencies(database, "hostA", targetID, []string{"abc"}, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid dependency job ID") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestSplitAndFormatDependencies(t *testing.T) {
	values := splitDependencyValues("1, 2 ,\t3")
	if len(values) != 3 || values[1] != "2" {
		t.Fatalf("unexpected split result: %#v", values)
	}
	if result := splitDependencyValues("   "); result != nil {
		t.Fatalf("expected nil result for empty string, got %#v", result)
	}

	deps := []queueDependency{
		{JobID: 10, AllowFailure: false},
		{JobID: 11, AllowFailure: true},
	}
	formatted := formatQueueDependencies(deps)
	if !strings.Contains(formatted, "10 (success)") || !strings.Contains(formatted, "11 (completion)") {
		t.Fatalf("unexpected format: %s", formatted)
	}
	if formatQueueDependencies(nil) != "" {
		t.Fatalf("expected empty string for empty deps")
	}
}

func TestDecodeQueueDependencies(t *testing.T) {
	spec := "10,11:any,foo"
	deps := decodeQueueDependencies(spec)
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps, got %d", len(deps))
	}
	if deps[0].JobID != 10 || deps[0].AllowFailure {
		t.Fatalf("unexpected first dep: %+v", deps[0])
	}
	if deps[1].JobID != 11 || !deps[1].AllowFailure {
		t.Fatalf("unexpected second dep: %+v", deps[1])
	}
	if out := formatQueueDependencies(decodeQueueDependencies("")); out != "" {
		t.Fatalf("expected empty decode, got %s", out)
	}
}
