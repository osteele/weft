package progress

import (
	"testing"
)

func TestParseProgress(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		want    *Progress
		wantNil bool
	}{
		// Percent patterns
		{
			name: "percent with percent sign",
			line: "Progress: 75%",
			want: &Progress{Percent: 75, RawLine: "Progress: 75%"},
		},
		{
			name: "percent with space before percent sign",
			line: "Progress: 75 %",
			want: &Progress{Percent: 75, RawLine: "Progress: 75 %"},
		},
		{
			name: "percent zero",
			line: "Progress: 0%",
			want: &Progress{Percent: 0, RawLine: "Progress: 0%"},
		},
		{
			name: "percent 100",
			line: "Progress: 100%",
			want: &Progress{Percent: 100, RawLine: "Progress: 100%"},
		},
		{
			name: "percent case insensitive",
			line: "progress: 50%",
			want: &Progress{Percent: 50, RawLine: "progress: 50%"},
		},

		// Slash patterns (N/M)
		{
			name: "slash format",
			line: "Progress: 9/14",
			want: &Progress{Percent: -1, Current: 9, Total: 14, RawLine: "Progress: 9/14"},
		},
		{
			name: "slash format with spaces",
			line: "Progress: 9 / 14",
			want: &Progress{Percent: -1, Current: 9, Total: 14, RawLine: "Progress: 9 / 14"},
		},
		{
			name: "slash format first item",
			line: "Progress: 1/10",
			want: &Progress{Percent: -1, Current: 1, Total: 10, RawLine: "Progress: 1/10"},
		},

		// "of" patterns
		{
			name: "of format",
			line: "Progress: 9 of 14",
			want: &Progress{Percent: -1, Current: 9, Total: 14, RawLine: "Progress: 9 of 14"},
		},
		{
			name: "out of format",
			line: "Progress: 9 out of 14",
			want: &Progress{Percent: -1, Current: 9, Total: 14, RawLine: "Progress: 9 out of 14"},
		},

		// tqdm patterns
		{
			name: "tqdm basic",
			line: "  2%|▏         | 50/2500 [00:30<25:00, 1.63it/s]",
			want: &Progress{Percent: 2, RawLine: "2%|▏         | 50/2500 [00:30<25:00, 1.63it/s]"},
		},
		{
			name: "tqdm with epoch prefix",
			line: "Epoch 1/3:  45%|████▌     | 450/1000 [01:23<01:42, 5.38it/s]",
			want: &Progress{Percent: 45, RawLine: "Epoch 1/3:  45%|████▌     | 450/1000 [01:23<01:42, 5.38it/s]"},
		},
		{
			name: "tqdm 100%",
			line: "100%|██████████| 500/500 [00:45<00:00, 11.11it/s, loss=0.234]",
			want: &Progress{Percent: 100, RawLine: "100%|██████████| 500/500 [00:45<00:00, 11.11it/s, loss=0.234]"},
		},
		{
			name: "tqdm 0%",
			line: "  0%|          | 0/1000 [00:00<?, ?it/s]",
			want: &Progress{Percent: 0, RawLine: "0%|          | 0/1000 [00:00<?, ?it/s]"},
		},

		// Epoch patterns
		{
			name: "epoch basic",
			line: "Epoch 3/10",
			want: &Progress{Percent: -1, Current: 3, Total: 10, RawLine: "Epoch 3/10"},
		},
		{
			name: "epoch with brackets",
			line: "Epoch [3/10]",
			want: &Progress{Percent: -1, Current: 3, Total: 10, RawLine: "Epoch [3/10]"},
		},
		{
			name: "epoch with trailing colon",
			line: "Epoch 3/10:",
			want: &Progress{Percent: -1, Current: 3, Total: 10, RawLine: "Epoch 3/10:"},
		},
		{
			name: "epoch case insensitive",
			line: "epoch 5/20",
			want: &Progress{Percent: -1, Current: 5, Total: 20, RawLine: "epoch 5/20"},
		},

		// Priority: Progress: patterns take precedence over tqdm/epoch
		{
			name: "Progress: preferred over epoch in same line",
			line: "Progress: 75%",
			want: &Progress{Percent: 75, RawLine: "Progress: 75%"},
		},

		// Edge cases
		{
			name:    "no match - random text",
			line:    "Some random text",
			wantNil: true,
		},
		{
			name:    "no match - empty line",
			line:    "",
			wantNil: true,
		},
		{
			name:    "no match - progress without colon",
			line:    "Progress 50%",
			wantNil: true,
		},
		{
			name: "with leading whitespace",
			line: "  Progress: 50%",
			want: &Progress{Percent: 50, RawLine: "Progress: 50%"},
		},
		{
			name: "with prefix text",
			line: "2026-01-10 12:00:00 Progress: 57%",
			want: &Progress{Percent: 57, RawLine: "Progress: 57%"},
		},
		{
			name: "with carriage return updates",
			line: "Progress: 10%\rProgress: 57%",
			want: &Progress{Percent: 57, RawLine: "Progress: 57%"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseProgress(tt.line)

			if tt.wantNil {
				if got != nil {
					t.Errorf("ParseProgress() = %+v, want nil", got)
				}
				return
			}

			if got == nil {
				t.Errorf("ParseProgress() = nil, want %+v", tt.want)
				return
			}

			if got.Percent != tt.want.Percent {
				t.Errorf("Percent = %d, want %d", got.Percent, tt.want.Percent)
			}
			if got.Current != tt.want.Current {
				t.Errorf("Current = %d, want %d", got.Current, tt.want.Current)
			}
			if got.Total != tt.want.Total {
				t.Errorf("Total = %d, want %d", got.Total, tt.want.Total)
			}
			if got.RawLine != tt.want.RawLine {
				t.Errorf("RawLine = %q, want %q", got.RawLine, tt.want.RawLine)
			}
		})
	}
}

func TestProgress_DisplayPercent(t *testing.T) {
	tests := []struct {
		name string
		prog Progress
		want int
	}{
		{
			name: "direct percent",
			prog: Progress{Percent: 75},
			want: 75,
		},
		{
			name: "from current/total",
			prog: Progress{Percent: -1, Current: 5, Total: 10},
			want: 50,
		},
		{
			name: "from current/total - partial",
			prog: Progress{Percent: -1, Current: 1, Total: 3},
			want: 33,
		},
		{
			name: "zero total",
			prog: Progress{Percent: -1, Current: 5, Total: 0},
			want: -1,
		},
		{
			name: "zero percent is valid",
			prog: Progress{Percent: 0},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.prog.DisplayPercent(); got != tt.want {
				t.Errorf("DisplayPercent() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFindLastProgress(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    *Progress
		wantNil bool
	}{
		{
			name: "single progress line",
			content: `Starting job...
Progress: 50%
Still working...`,
			want: &Progress{Percent: 50, RawLine: "Progress: 50%"},
		},
		{
			name: "multiple progress lines - returns last",
			content: `Starting job...
Progress: 25%
Working...
Progress: 50%
More work...
Progress: 75%
Almost done...`,
			want: &Progress{Percent: 75, RawLine: "Progress: 75%"},
		},
		{
			name: "progress at end",
			content: `Starting job...
Progress: 100%`,
			want: &Progress{Percent: 100, RawLine: "Progress: 100%"},
		},
		{
			name:    "no progress lines",
			content: "Just some output\nNo progress here\n",
			wantNil: true,
		},
		{
			name:    "empty content",
			content: "",
			wantNil: true,
		},
		{
			name: "mixed formats - returns last",
			content: `Progress: 1/10
Progress: 2/10
Progress: 30%`,
			want: &Progress{Percent: 30, RawLine: "Progress: 30%"},
		},
		{
			name: "tqdm progress in log output",
			content: `Loading model...
Epoch 1/3:  45%|████▌     | 450/1000 [01:23<01:42, 5.38it/s]
Some other output`,
			want: &Progress{Percent: 45, RawLine: "Epoch 1/3:  45%|████▌     | 450/1000 [01:23<01:42, 5.38it/s]"},
		},
		{
			name: "epoch-only progress",
			content: `Starting training...
Epoch 3/10
Training loss: 0.234`,
			want: &Progress{Percent: -1, Current: 3, Total: 10, RawLine: "Epoch 3/10"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindLastProgress(tt.content)

			if tt.wantNil {
				if got != nil {
					t.Errorf("FindLastProgress() = %+v, want nil", got)
				}
				return
			}

			if got == nil {
				t.Errorf("FindLastProgress() = nil, want %+v", tt.want)
				return
			}

			if got.Percent != tt.want.Percent {
				t.Errorf("Percent = %d, want %d", got.Percent, tt.want.Percent)
			}
			if got.RawLine != tt.want.RawLine {
				t.Errorf("RawLine = %q, want %q", got.RawLine, tt.want.RawLine)
			}
		})
	}
}

func TestTracker(t *testing.T) {
	tracker := NewTracker()

	// Test initial state
	if got := tracker.GetLastSize("/path/to/log"); got != 0 {
		t.Errorf("GetLastSize() for unknown path = %d, want 0", got)
	}

	// Test SetSize and GetLastSize
	tracker.SetSize("/path/to/log", 1000)
	if got := tracker.GetLastSize("/path/to/log"); got != 1000 {
		t.Errorf("GetLastSize() after SetSize = %d, want 1000", got)
	}

	// Test update
	tracker.SetSize("/path/to/log", 2000)
	if got := tracker.GetLastSize("/path/to/log"); got != 2000 {
		t.Errorf("GetLastSize() after update = %d, want 2000", got)
	}

	// Test Clear
	tracker.Clear("/path/to/log")
	if got := tracker.GetLastSize("/path/to/log"); got != 0 {
		t.Errorf("GetLastSize() after Clear = %d, want 0", got)
	}
}
