package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/orchestration"
)

func TestParseRebalanceInstanceScope(t *testing.T) {
	scope, err := parseRebalanceInstanceScope("wi10,11, wi12")
	if err != nil {
		t.Fatalf("parseRebalanceInstanceScope: %v", err)
	}
	for _, id := range []int64{10, 11, 12} {
		if _, ok := scope[id]; !ok {
			t.Fatalf("scope missing %d", id)
		}
	}
}

func TestParseRebalanceJobScope(t *testing.T) {
	scope, err := parseRebalanceJobScope("wj10:12,15")
	if err != nil {
		t.Fatalf("parseRebalanceJobScope: %v", err)
	}
	for _, id := range []int64{10, 11, 12, 15} {
		if _, ok := scope[id]; !ok {
			t.Fatalf("scope missing %d", id)
		}
	}
}

func TestPrintRebalanceMovesTable(t *testing.T) {
	moves := []orchestration.QueueRebalanceMove{
		{JobID: 1209, FromInstanceID: 1088, ToInstanceID: 1093, CostRatio: 1.00, Reason: "dst idle, same class, same $"},
	}
	var buf bytes.Buffer
	printRebalanceMovesTable(&buf, moves, true)
	out := buf.String()
	for _, want := range []string{
		"OK",
		"JOB",
		"FROM",
		"TO",
		"RATIO",
		"wj1209",
		"wi1088",
		"wi1093",
		"1.00",
		"dst idle, same class, same $",
		"✓",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}
