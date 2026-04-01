package terminal

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestPrintJobsJSON(t *testing.T) {
	jobs := []*db.Job{
		{
			ID:          42,
			Host:        "cool30",
			Status:      db.StatusCompleted,
			StartTime:   1711900000,
			WorkingDir:  "/workspace/myproject",
			Project:     "myproject",
			Description: "train model",
			ExitCode:    testIntPtr(0),
		},
		{
			ID:          43,
			Host:        "",
			Status:      db.StatusQueued,
			Description: "queued job",
		},
	}

	cols, err := resolveColumns(nil, defaultJSONColumnKeys)
	if err != nil {
		t.Fatalf("resolveColumns: %v", err)
	}

	var buf bytes.Buffer
	if err := printJobsJSON(&buf, jobs, cols); err != nil {
		t.Fatalf("printJobsJSON: %v", err)
	}

	var records []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &records); err != nil {
		t.Fatalf("invalid JSON output: %v\n%s", err, buf.String())
	}

	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}

	// Check first record has expected keys
	rec := records[0]
	for _, key := range []string{"id", "host", "status", "started", "project", "description"} {
		if _, ok := rec[key]; !ok {
			t.Errorf("missing key %q in JSON record", key)
		}
	}

	// Check id is a number
	if id, ok := rec["id"].(float64); !ok || id != 42 {
		t.Errorf("id = %v, want 42", rec["id"])
	}

	// Check exit_code is a number
	if ec, ok := rec["exit_code"].(float64); !ok || ec != 0 {
		t.Errorf("exit_code = %v, want 0", rec["exit_code"])
	}

	// Check null exit_code for job without one
	if records[1]["exit_code"] != nil {
		t.Errorf("exit_code for queued job = %v, want nil", records[1]["exit_code"])
	}
}

func TestPrintJobsTSV(t *testing.T) {
	jobs := []*db.Job{
		{
			ID:          42,
			Host:        "cool30",
			Status:      db.StatusCompleted,
			StartTime:   1711900000,
			WorkingDir:  "/workspace/myproject",
			Project:     "myproject",
			Description: "train model",
			ExitCode:    testIntPtr(0),
		},
	}

	cols, err := resolveColumns(nil, defaultTSVColumnKeys)
	if err != nil {
		t.Fatalf("resolveColumns: %v", err)
	}

	var buf bytes.Buffer
	if err := printJobsTSV(&buf, jobs, cols); err != nil {
		t.Fatalf("printJobsTSV: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (header + 1 row), got %d:\n%s", len(lines), buf.String())
	}

	// Check header
	headerFields := strings.Split(lines[0], "\t")
	if headerFields[0] != "ID" {
		t.Errorf("first header = %q, want ID", headerFields[0])
	}

	// Check row has same number of fields as header
	rowFields := strings.Split(lines[1], "\t")
	if len(rowFields) != len(headerFields) {
		t.Errorf("row has %d fields, header has %d", len(rowFields), len(headerFields))
	}
}

func TestPrintJobsTSVSanitizesTabs(t *testing.T) {
	jobs := []*db.Job{
		{
			ID:          1,
			Description: "has\ttab\tin\tit",
			Status:      db.StatusCompleted,
		},
	}

	cols, err := resolveColumns([]string{"id", "description"}, defaultTSVColumnKeys)
	if err != nil {
		t.Fatalf("resolveColumns: %v", err)
	}

	var buf bytes.Buffer
	if err := printJobsTSV(&buf, jobs, cols); err != nil {
		t.Fatalf("printJobsTSV: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	// Row should have exactly 2 tab-separated fields (id + description)
	rowFields := strings.Split(lines[1], "\t")
	if len(rowFields) != 2 {
		t.Errorf("expected 2 fields, got %d: %q", len(rowFields), lines[1])
	}
}

func TestResolveColumnsValid(t *testing.T) {
	cols, err := resolveColumns([]string{"id", "host", "status"}, defaultTableColumnKeys)
	if err != nil {
		t.Fatalf("resolveColumns: %v", err)
	}
	if len(cols) != 3 {
		t.Fatalf("expected 3 columns, got %d", len(cols))
	}
	if cols[0].key != "id" {
		t.Errorf("first column = %q, want id", cols[0].key)
	}
}

func TestResolveColumnsInvalid(t *testing.T) {
	_, err := resolveColumns([]string{"id", "bogus"}, defaultTableColumnKeys)
	if err == nil {
		t.Fatal("expected error for invalid column name")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error = %v, want mention of 'bogus'", err)
	}
}

func TestResolveColumnsDefault(t *testing.T) {
	cols, err := resolveColumns(nil, defaultJSONColumnKeys)
	if err != nil {
		t.Fatalf("resolveColumns: %v", err)
	}
	if len(cols) != len(defaultJSONColumnKeys) {
		t.Fatalf("expected %d columns, got %d", len(defaultJSONColumnKeys), len(cols))
	}
}

func TestRenderJobListPlainNoTruncate(t *testing.T) {
	longDesc := strings.Repeat("x", 200)
	jobs := []*db.Job{
		{
			ID:          42,
			Host:        "cool30",
			Status:      db.StatusRunning,
			StartTime:   1,
			Description: longDesc,
		},
	}

	out := renderJobListPlainWithOptions(jobs, 80, nil, true)
	if strings.Contains(out, "…") {
		t.Fatalf("no-truncate output should not contain ellipsis:\n%s", out)
	}
	if !strings.Contains(out, longDesc) {
		t.Fatalf("no-truncate output should contain full description:\n%s", out)
	}
}

func TestRenderJobListPlainWithColumns(t *testing.T) {
	jobs := []*db.Job{
		{
			ID:          42,
			Host:        "cool30",
			Status:      db.StatusRunning,
			StartTime:   1,
			Description: "test job",
		},
	}

	out := renderJobListPlainWithOptions(jobs, 120, []string{"id", "status", "description"}, false)
	if !strings.Contains(out, "ID") {
		t.Fatalf("output missing ID header:\n%s", out)
	}
	if !strings.Contains(out, "STATUS") {
		t.Fatalf("output missing STATUS header:\n%s", out)
	}
	// Should NOT contain HOST since we only selected id, status, description
	if strings.Contains(out, "HOST") {
		t.Fatalf("output should not contain HOST header:\n%s", out)
	}
}
