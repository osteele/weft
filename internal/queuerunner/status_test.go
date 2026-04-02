package queuerunner

import (
	"reflect"
	"testing"
)

func TestParseStatus(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   StatusInfo
	}{
		{
			name: "all fields present",
			output: `RUNNER:yes
CURRENT:123
DEPTH:5
STOP:no`,
			want: StatusInfo{
				RunnerActive:   true,
				CurrentJob:     "123",
				QueuedJobCount: 5,
				StopPending:    false,
			},
		},
		{
			name: "runner inactive",
			output: `RUNNER:no
CURRENT:
DEPTH:0
STOP:no`,
			want: StatusInfo{
				RunnerActive:   false,
				CurrentJob:     "",
				QueuedJobCount: 0,
				StopPending:    false,
			},
		},
		{
			name: "runner active with stop pending",
			output: `RUNNER:yes
CURRENT:456
DEPTH:3
STOP:yes`,
			want: StatusInfo{
				RunnerActive:   true,
				CurrentJob:     "456",
				QueuedJobCount: 3,
				StopPending:    true,
			},
		},
		{
			name: "blocked reasons",
			output: `RUNNER:yes
CURRENT:
DEPTH:2
BLOCKED:554:benchmark gate: cpu=9%>5%
BLOCKED:555:gpu gate: all a100 GPUs are in use
STOP:no`,
			want: StatusInfo{
				RunnerActive:   true,
				QueuedJobCount: 2,
				BlockedReasons: map[int64]string{
					554: "benchmark gate: cpu=9%>5%",
					555: "gpu gate: all a100 GPUs are in use",
				},
				StopPending: false,
			},
		},
		{
			name:   "empty output",
			output: "",
			want: StatusInfo{
				RunnerActive:   false,
				CurrentJob:     "",
				QueuedJobCount: 0,
				StopPending:    false,
			},
		},
		{
			name: "whitespace handling",
			output: `  RUNNER:yes
  CURRENT:  789
  DEPTH:  10
  STOP:no  `,
			want: StatusInfo{
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
			if !reflect.DeepEqual(got.BlockedReasons, tt.want.BlockedReasons) {
				t.Errorf("BlockedReasons = %#v, want %#v", got.BlockedReasons, tt.want.BlockedReasons)
			}
		})
	}
}
