package db

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestTerminationReasonConstantsMatchSchema enforces set-equality between the
// exported TerminationReason* constants in cloud_instances.go and the
// launches_termination_reason_check CHECK list. Drift between the two surfaces
// at runtime as "CHECK constraint failed: launches_termination_reason_check"
// when the reconciler writes a Go-side reason that the schema does not allow,
// and silently jams the affected instance in status='running' forever. This
// test catches that drift at build time.
//
// To add a new termination reason: add the constant to cloud_instances.go AND
// add a migration that REPLACEs the IN-list to include the new value (see
// migrations/sql/00002_add_provider_timeout_reason.sql and
// 00008_add_upload_stall_reason.sql for the pattern).
func TestTerminationReasonConstantsMatchSchema(t *testing.T) {
	want := map[string]struct{}{
		TerminationReasonCompleted:        {},
		TerminationReasonProviderFailure:  {},
		TerminationReasonJobFailure:       {},
		TerminationReasonDiskFull:         {},
		TerminationReasonInfraFailure:     {},
		TerminationReasonBootstrapTimeout: {},
		TerminationReasonCancelled:        {},
		TerminationReasonPhaseStall:       {},
		TerminationReasonPreempted:        {},
		TerminationReasonUnknown:          {},
		TerminationReasonWeftBug:          {},
		TerminationReasonProviderTimeout:  {},
		TerminationReasonUploadStall:      {},
	}

	database := setupTestDB(t)

	var createSQL string
	if err := database.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'launches'`,
	).Scan(&createSQL); err != nil {
		t.Fatalf("read launches schema: %v", err)
	}

	got, err := extractTerminationReasonsFromCheck(createSQL)
	if err != nil {
		t.Fatalf("extract CHECK list: %v\nschema text:\n%s", err, createSQL)
	}

	// Set-equality, both directions.
	var missingInSchema, missingInCode []string
	for r := range want {
		if _, ok := got[r]; !ok {
			missingInSchema = append(missingInSchema, r)
		}
	}
	for r := range got {
		if _, ok := want[r]; !ok {
			missingInCode = append(missingInCode, r)
		}
	}
	sort.Strings(missingInSchema)
	sort.Strings(missingInCode)

	if len(missingInSchema) > 0 {
		t.Errorf("termination reasons defined in Go but missing from launches_termination_reason_check: %v\n"+
			"add a migration like internal/db/migrations/sql/00008_add_upload_stall_reason.sql",
			missingInSchema)
	}
	if len(missingInCode) > 0 {
		t.Errorf("termination reasons in launches_termination_reason_check but missing from Go constants: %v\n"+
			"add the constant to internal/db/cloud_instances.go and update this test",
			missingInCode)
	}
}

// extractTerminationReasonsFromCheck pulls the IN-list values out of the
// launches_termination_reason_check CHECK clause in the stored CREATE TABLE
// text. The clause has the shape:
//
//	CONSTRAINT launches_termination_reason_check CHECK (termination_reason IS NULL OR termination_reason IN ('a', 'b', ...))
func extractTerminationReasonsFromCheck(createSQL string) (map[string]struct{}, error) {
	re := regexp.MustCompile(`(?is)launches_termination_reason_check\s+CHECK\s*\(\s*termination_reason\s+IS\s+NULL\s+OR\s+termination_reason\s+IN\s*\(([^)]*)\)`)
	m := re.FindStringSubmatch(createSQL)
	if m == nil {
		return nil, &extractError{msg: "could not locate launches_termination_reason_check IN-list"}
	}
	inList := m[1]

	result := make(map[string]struct{})
	// Each value is single-quoted; pull them out tolerantly.
	for _, part := range strings.Split(inList, ",") {
		v := strings.TrimSpace(part)
		v = strings.Trim(v, "'")
		if v == "" {
			continue
		}
		result[v] = struct{}{}
	}
	if len(result) == 0 {
		return nil, &extractError{msg: "CHECK IN-list parsed as empty"}
	}
	return result, nil
}

type extractError struct{ msg string }

func (e *extractError) Error() string { return e.msg }
