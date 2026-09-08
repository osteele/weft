package cmd

import (
	"errors"
	"strings"
	"testing"
)

func TestQueueAgentVersionLines(t *testing.T) {
	cases := []struct {
		name         string
		running      versionReading
		deployed     versionReading
		wantLines    int
		wantContains []string
		wantOmits    []string
	}{
		{
			name:         "matching versions agree silently",
			running:      versionReading{version: "abc123def456"},
			deployed:     versionReading{version: "abc123def456"},
			wantLines:    1,
			wantContains: []string{"running abc123def456", "deployed abc123def456"},
			wantOmits:    []string{"MISMATCH"},
		},
		{
			name:         "disagreement is stated in words",
			running:      versionReading{version: "aaa111bbb222"},
			deployed:     versionReading{version: "ccc333ddd444"},
			wantLines:    2,
			wantContains: []string{"MISMATCH", "aaa111bbb222", "ccc333ddd444", "queue-host", "queue update queue-host"},
			wantOmits:    nil,
		},
		{
			name:         "running version absent is unknown, never a match or mismatch",
			running:      versionReading{absent: true, note: "not reported by the running agent"},
			deployed:     versionReading{version: "ccc333ddd444"},
			wantLines:    1,
			wantContains: []string{"running unknown (not reported by the running agent)"},
			wantOmits:    []string{"MISMATCH", "running ccc333ddd444"},
		},
		{
			name:         "deployed unreadable is unknown, never a mismatch",
			running:      versionReading{version: "aaa111bbb222"},
			deployed:     versionReading{err: errors.New("ssh: connection refused")},
			wantLines:    1,
			wantContains: []string{"deployed unknown (ssh: connection refused)"},
			wantOmits:    []string{"MISMATCH"},
		},
		{
			name:         "running state unreadable is unknown, never a mismatch",
			running:      versionReading{err: errors.New("read R2 runner state for queue-host: unavailable")},
			deployed:     versionReading{version: "ccc333ddd444"},
			wantLines:    1,
			wantContains: []string{"running unknown (read R2 runner state for queue-host: unavailable)"},
			wantOmits:    []string{"MISMATCH"},
		},
		{
			name:         "deployed binary not installed is its own state",
			running:      versionReading{version: "aaa111bbb222"},
			deployed:     versionReading{absent: true, note: "not installed"},
			wantLines:    1,
			wantContains: []string{"deployed unknown (not installed)"},
			wantOmits:    []string{"MISMATCH"},
		},
		{
			name:         "stale publication qualifies but keeps the version",
			running:      versionReading{version: "aaa111bbb222", note: "state published 5m ago"},
			deployed:     versionReading{version: "ccc333ddd444"},
			wantLines:    2,
			wantContains: []string{"running aaa111bbb222 (state published 5m ago)", "MISMATCH"},
			wantOmits:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := queueAgentVersionLines("queue-host", tc.running, tc.deployed)
			if len(lines) != tc.wantLines {
				t.Fatalf("got %d lines (%v), want %d", len(lines), lines, tc.wantLines)
			}
			joined := strings.Join(lines, "\n")
			for _, want := range tc.wantContains {
				if !strings.Contains(joined, want) {
					t.Errorf("output missing %q, got:\n%s", want, joined)
				}
			}
			for _, omit := range tc.wantOmits {
				if strings.Contains(joined, omit) {
					t.Errorf("output must not contain %q, got:\n%s", omit, joined)
				}
			}
		})
	}
}
