package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
)

// columnDef defines a displayable column for job list output.
type columnDef struct {
	key        string
	title      string // header label for table/TSV
	jsonKey    string // JSON object key (empty = same as key)
	width      int    // default width for table mode
	alignRight bool
	value      func(*db.Job) string
	// jsonValue returns a typed value for JSON output.
	// If nil, the string from value() is used.
	jsonValue func(*db.Job) any
}

func (c columnDef) effectiveJSONKey() string {
	if c.jsonKey != "" {
		return c.jsonKey
	}
	return c.key
}

// allColumnDefs returns all available column definitions.
func allColumnDefs() []columnDef {
	return []columnDef{
		{
			key: "check", title: "", width: 2,
			value: func(job *db.Job) string {
				if job.HasTag(db.ProcessedTag) {
					return "✓"
				}
				return " "
			},
		},
		{
			key: "id", title: "ID", width: 6, alignRight: true,
			value:     func(job *db.Job) string { return fmt.Sprintf("%d", job.ID) },
			jsonValue: func(job *db.Job) any { return job.ID },
		},
		{
			key: "host", title: "HOST", width: 12,
			value: func(job *db.Job) string { return formatJobListHost(job) },
		},
		{
			key: "status", title: "STATUS", width: 14,
			value: func(job *db.Job) string { return formatJobListStatus(job) },
		},
		{
			key: "started", title: "STARTED", width: 11,
			value: func(job *db.Job) string { return formatJobListStarted(job) },
			jsonValue: func(job *db.Job) any {
				if job.StartTime <= 0 {
					return nil
				}
				return time.Unix(job.StartTime, 0).Format(time.RFC3339)
			},
		},
		{
			key: "project", title: "PROJECT", width: 0, // computed from data
			value: func(job *db.Job) string { return formatJobListProject(job) },
		},
		{
			key: "dir", title: "DIR", width: 14,
			value: func(job *db.Job) string { return job.DirectoryTailDisplay() },
		},
		{
			key: "description", title: "DESCRIPTION", width: 0, // fills remaining space
			value: func(job *db.Job) string { return job.EffectiveDescription() },
		},
		{
			key: "command", title: "COMMAND", width: 0,
			value: func(job *db.Job) string { return job.EffectiveCommand() },
		},
		{
			key: "exit_code", title: "EXIT", jsonKey: "exit_code", width: 6, alignRight: true,
			value: func(job *db.Job) string {
				if job.ExitCode == nil {
					return ""
				}
				return fmt.Sprintf("%d", *job.ExitCode)
			},
			jsonValue: func(job *db.Job) any {
				if job.ExitCode == nil {
					return nil
				}
				return *job.ExitCode
			},
		},
		{
			key: "duration", title: "DURATION", width: 10,
			value: func(job *db.Job) string {
				if job.EndTime == nil || job.StartTime <= 0 {
					return ""
				}
				return db.FormatDuration(*job.EndTime - job.StartTime)
			},
		},
		{
			key: "tags", title: "TAGS", width: 20,
			value: func(job *db.Job) string {
				return strings.Join(job.DisplayTags(), ",")
			},
		},
		{
			key: "gpu", title: "GPU", width: 10,
			value: func(job *db.Job) string {
				if dev := job.GPUDevice(); dev != "" {
					return dev
				}
				return job.GPUClass
			},
		},
	}
}

// columnDefMap returns a map of key -> columnDef for quick lookup.
func columnDefMap() map[string]columnDef {
	m := make(map[string]columnDef)
	for _, c := range allColumnDefs() {
		m[c.key] = c
	}
	return m
}

// defaultTableColumnKeys returns the column keys used in the default table
// layout at the widest terminal width (>= 96).
var defaultTableColumnKeys = []string{"check", "id", "host", "status", "started", "project", "dir", "description"}

// defaultJSONColumnKeys returns the column keys used in JSON output by default.
var defaultJSONColumnKeys = []string{"id", "host", "status", "started", "project", "dir", "description", "command", "exit_code", "duration", "tags", "gpu"}

// defaultTSVColumnKeys returns the column keys used in TSV output by default
// (same as the wide table but without the checkmark column).
var defaultTSVColumnKeys = []string{"id", "host", "status", "started", "project", "dir", "description"}

// resolveColumns validates and resolves column names to columnDefs.
// If keys is nil or empty, defaultKeys is used.
func resolveColumns(keys []string, defaultKeys []string) ([]columnDef, error) {
	if len(keys) == 0 {
		keys = defaultKeys
	}
	m := columnDefMap()
	cols := make([]columnDef, 0, len(keys))
	for _, k := range keys {
		c, ok := m[k]
		if !ok {
			valid := make([]string, 0, len(m))
			for _, cd := range allColumnDefs() {
				valid = append(valid, cd.key)
			}
			return nil, fmt.Errorf("unknown column %q (valid: %s)", k, strings.Join(valid, ", "))
		}
		cols = append(cols, c)
	}
	return cols, nil
}
