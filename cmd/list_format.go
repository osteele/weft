package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/osteele/weft/internal/db"
)

// printJobsJSON writes jobs as a JSON array to w.
func printJobsJSON(w io.Writer, jobs []*db.Job, cols []columnDef) error {
	records := make([]map[string]any, 0, len(jobs))
	for _, job := range jobs {
		rec := make(map[string]any, len(cols))
		for _, c := range cols {
			key := c.effectiveJSONKey()
			if c.jsonValue != nil {
				rec[key] = c.jsonValue(job)
			} else {
				rec[key] = c.value(job)
			}
		}
		records = append(records, rec)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(records)
}

// printJobsTSV writes jobs as tab-separated values to w.
func printJobsTSV(w io.Writer, jobs []*db.Job, cols []columnDef) error {
	// Header
	titles := make([]string, len(cols))
	for i, c := range cols {
		titles[i] = c.title
		if titles[i] == "" {
			titles[i] = strings.ToUpper(c.key)
		}
	}
	if _, err := fmt.Fprintln(w, strings.Join(titles, "\t")); err != nil {
		return err
	}

	// Rows
	for _, job := range jobs {
		values := make([]string, len(cols))
		for i, c := range cols {
			values[i] = sanitizeTSV(c.value(job))
		}
		if _, err := fmt.Fprintln(w, strings.Join(values, "\t")); err != nil {
			return err
		}
	}
	return nil
}

// sanitizeTSV replaces tabs and newlines in a value for safe TSV output.
func sanitizeTSV(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	return s
}
