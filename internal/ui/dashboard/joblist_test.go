package dashboard

import "testing"

func TestAbbreviateProject(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		want     string
	}{
		// Fits as-is
		{"fits exactly", "adaptive-storage-placement", 26, "adaptive-storage-placement"},
		{"fits with room", "adaptive-storage-placement", 30, "adaptive-storage-placement"},

		// Progressive shortening of 3-segment name
		{"3seg width 14", "adaptive-storage-placement", 14, "adap-stor-plac"},
		{"3seg width 11", "adaptive-storage-placement", 11, "ada-sto-pla"},
		{"3seg width 8", "adaptive-storage-placement", 8, "ad-st-pl"},
		{"3seg width 5", "adaptive-storage-placement", 5, "a-s-p"},
		{"3seg width 3", "adaptive-storage-placement", 3, "asp"},
		{"3seg width 2", "adaptive-storage-placement", 2, "a…"},

		// Progressive shortening of 2-segment name
		{"2seg fits", "markov-attention", 16, "markov-attention"},
		{"2seg width 9", "markov-attention", 9, "mark-attn"},
		{"2seg width 7", "markov-attention", 7, "mar-att"},
		{"2seg width 3", "markov-attention", 3, "m-a"},
		{"2seg width 2", "markov-attention", 2, "ma"},

		// Unequal segments: preserves shorter segments
		{"unequal width 12", "head-type-ontology", 12, "head-typ-ont"},
		{"unequal width 8", "head-type-ontology", 8, "he-ty-on"},
		{"unequal width 5", "head-type-ontology", 5, "h-t-o"},
		{"unequal width 3", "head-type-ontology", 3, "hto"},

		// Non-hyphenated names
		{"short no hyphen", "LM2", 3, "LM2"},
		{"short no hyphen truncate", "LM2", 2, "L…"},

		// Edge cases
		{"empty string", "", 10, ""},
		{"maxWidth 0", "test", 0, ""},
		{"maxWidth 1", "adaptive-storage", 1, "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AbbreviateProject(tt.input, tt.maxWidth)
			if got != tt.want {
				t.Errorf("AbbreviateProject(%q, %d) = %q, want %q", tt.input, tt.maxWidth, got, tt.want)
			}
		})
	}
}

func TestAbbreviateProjectPrefersReadableStems(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		want     string
	}{
		{"llm performance long", "llm-performance-mode", 13, "llm-perf-mode"},
		{"llm performance short", "llm-performance-mode", 10, "llm-per-mo"},
		{"markov attention long", "markov-attention", 13, "markov-attent"},
		{"markov attention short", "markov-attention", 9, "mark-attn"},
		{"role encoder injection long", "role-encoder-injection", 15, "role-enc-inject"},
		{"role encoder injection short", "role-encoder-injection", 12, "role-enc-inj"},
		{"structural probes long", "structural-probes", 13, "struct-probes"},
		{"structural probes short", "structural-probes", 11, "struct-prob"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AbbreviateProject(tt.input, tt.maxWidth)
			if got != tt.want {
				t.Errorf("AbbreviateProject(%q, %d) = %q, want %q", tt.input, tt.maxWidth, got, tt.want)
			}
		})
	}
}

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
