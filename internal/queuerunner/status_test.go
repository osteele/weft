package queuerunner

import "testing"

func TestParseStatus(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   StatusInfo
	}{
		{
			name: "all fields present with jq available",
			output: `JQ:yes
RUNNER:yes
CURRENT:123
DEPTH:5
STOP:no`,
			want: StatusInfo{
				JqMissing:      false,
				RunnerActive:   true,
				CurrentJob:     "123",
				QueuedJobCount: 5,
				StopPending:    false,
			},
		},
		{
			name: "jq missing",
			output: `JQ:no
RUNNER:no
CURRENT:
DEPTH:0
STOP:no`,
			want: StatusInfo{
				JqMissing:      true,
				RunnerActive:   false,
				CurrentJob:     "",
				QueuedJobCount: 0,
				StopPending:    false,
			},
		},
		{
			name: "runner active with stop pending",
			output: `JQ:yes
RUNNER:yes
CURRENT:456
DEPTH:3
STOP:yes`,
			want: StatusInfo{
				JqMissing:      false,
				RunnerActive:   true,
				CurrentJob:     "456",
				QueuedJobCount: 3,
				StopPending:    true,
			},
		},
		{
			name: "runner inactive, no current job",
			output: `JQ:yes
RUNNER:no
CURRENT:
DEPTH:0
STOP:no`,
			want: StatusInfo{
				JqMissing:      false,
				RunnerActive:   false,
				CurrentJob:     "",
				QueuedJobCount: 0,
				StopPending:    false,
			},
		},
		{
			name:   "empty output",
			output: "",
			want: StatusInfo{
				JqMissing:      false, // default when JQ line is missing
				RunnerActive:   false,
				CurrentJob:     "",
				QueuedJobCount: 0,
				StopPending:    false,
			},
		},
		{
			name:   "partial output (only jq status)",
			output: `JQ:no`,
			want: StatusInfo{
				JqMissing:      true,
				RunnerActive:   false,
				CurrentJob:     "",
				QueuedJobCount: 0,
				StopPending:    false,
			},
		},
		{
			name: "whitespace handling",
			output: `  JQ:yes
  RUNNER:yes
  CURRENT:  789
  DEPTH:  10
  STOP:no  `,
			want: StatusInfo{
				JqMissing:      false,
				RunnerActive:   true,
				CurrentJob:     "789",
				QueuedJobCount: 10,
				StopPending:    false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseStatus(tt.output)
			if got.JqMissing != tt.want.JqMissing {
				t.Errorf("JqMissing = %v, want %v", got.JqMissing, tt.want.JqMissing)
			}
			if got.RunnerActive != tt.want.RunnerActive {
				t.Errorf("RunnerActive = %v, want %v", got.RunnerActive, tt.want.RunnerActive)
			}
			if got.CurrentJob != tt.want.CurrentJob {
				t.Errorf("CurrentJob = %q, want %q", got.CurrentJob, tt.want.CurrentJob)
			}
			if got.QueuedJobCount != tt.want.QueuedJobCount {
				t.Errorf("QueuedJobCount = %d, want %d", got.QueuedJobCount, tt.want.QueuedJobCount)
			}
			if got.StopPending != tt.want.StopPending {
				t.Errorf("StopPending = %v, want %v", got.StopPending, tt.want.StopPending)
			}
		})
	}
}

func TestJqInstallCommand(t *testing.T) {
	cmd := JqInstallCommand("testhost")
	if cmd == "" {
		t.Error("JqInstallCommand returned empty string")
	}
	// Verify it contains essential parts
	if !contains(cmd, "testhost") {
		t.Error("JqInstallCommand should contain the hostname")
	}
	if !contains(cmd, "jq") {
		t.Error("JqInstallCommand should mention jq")
	}
	if !contains(cmd, ".local/bin") {
		t.Error("JqInstallCommand should install to ~/.local/bin")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
