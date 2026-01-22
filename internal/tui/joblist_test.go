package tui

import "testing"

func TestTruncateToWidth(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		want     string
	}{
		{
			name:     "ASCII within limit",
			input:    "hello",
			maxWidth: 10,
			want:     "hello",
		},
		{
			name:     "ASCII exactly at limit",
			input:    "hello",
			maxWidth: 5,
			want:     "hello",
		},
		{
			name:     "ASCII exceeds limit",
			input:    "hello world",
			maxWidth: 5,
			want:     "hello",
		},
		{
			name:     "empty string",
			input:    "",
			maxWidth: 10,
			want:     "",
		},
		{
			name:     "zero width",
			input:    "hello",
			maxWidth: 0,
			want:     "",
		},
		{
			name:     "negative width",
			input:    "hello",
			maxWidth: -1,
			want:     "",
		},
		{
			name:     "CJK characters within limit",
			input:    "你好",
			maxWidth: 10,
			want:     "你好",
		},
		{
			name:     "CJK characters at limit",
			input:    "你好",
			maxWidth: 4,
			want:     "你好",
		},
		{
			name:     "CJK truncated mid-character",
			input:    "你好世界",
			maxWidth: 5,
			want:     "你好",
		},
		{
			name:     "mixed ASCII and CJK",
			input:    "hi你好",
			maxWidth: 5,
			want:     "hi你",
		},
		{
			name:     "emoji (wide character)",
			input:    "hello🎉world",
			maxWidth: 7,
			want:     "hello🎉",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateToWidth(tt.input, tt.maxWidth)
			if got != tt.want {
				t.Errorf("truncateToWidth(%q, %d) = %q, want %q", tt.input, tt.maxWidth, got, tt.want)
			}
		})
	}
}

func TestTruncateOrPad(t *testing.T) {
	tests := []struct {
		name  string
		input string
		width int
		want  string
	}{
		{
			name:  "pad short string",
			input: "hi",
			width: 5,
			want:  "hi   ",
		},
		{
			name:  "exact width no change",
			input: "hello",
			width: 5,
			want:  "hello",
		},
		{
			name:  "truncate long string with ellipsis",
			input: "hello world",
			width: 5,
			want:  "hell…",
		},
		{
			name:  "CJK padding",
			input: "你好",
			width: 6,
			want:  "你好  ",
		},
		{
			name:  "CJK truncation",
			input: "你好世界",
			width: 5,
			want:  "你好…",
		},
		{
			name:  "mixed content truncation",
			input: "hi你好world",
			width: 6,
			want:  "hi你…", // "hi你" = 4 width, + "…" = 5 width fits in 6
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateOrPad(tt.input, tt.width)
			if got != tt.want {
				t.Errorf("truncateOrPad(%q, %d) = %q, want %q", tt.input, tt.width, got, tt.want)
			}
		})
	}
}
