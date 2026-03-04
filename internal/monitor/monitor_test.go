package monitor

import (
	"path/filepath"
	"testing"
)

func TestNaturalSortStrings(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{
			name:  "host2 before host10",
			input: []string{"host10", "host2", "host1"},
			want:  []string{"host1", "host2", "host10"},
		},
		{
			name:  "mixed alpha and digit",
			input: []string{"b2", "a10", "a2", "b1"},
			want:  []string{"a2", "a10", "b1", "b2"},
		},
		{
			name:  "empty slice",
			input: []string{},
			want:  []string{},
		},
		{
			name:  "single element",
			input: []string{"host1"},
			want:  []string{"host1"},
		},
		{
			name:  "pure alpha",
			input: []string{"charlie", "alpha", "bravo"},
			want:  []string{"alpha", "bravo", "charlie"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := make([]string, len(tt.input))
			copy(got, tt.input)
			naturalSortStrings(got)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %q, want %q (full: %v)", i, got[i], tt.want[i], got)
					break
				}
			}
		})
	}
}

func TestNaturalLess(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"digit vs digit different", "host2", "host10", true},
		{"digit vs digit same prefix", "host10", "host2", false},
		{"case insensitive", "Host", "host", true}, // equal after lowering → fallback to a < b, "H" < "h"
		{"case insensitive ordering", "Apple", "banana", true},
		{"equal strings", "abc", "abc", false},
		{"empty vs non-empty", "", "a", true},
		{"prefix shorter", "ab", "abc", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := naturalLess(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("naturalLess(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestSplitIntoSegments(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"alpha only", "abc", []string{"abc"}},
		{"digit only", "123", []string{"123"}},
		{"alpha then digit", "host10", []string{"host", "10"}},
		{"digit then alpha", "10abc", []string{"10", "abc"}},
		{"punctuation separates", "a-b", []string{"a", "-", "b"}},
		{"mixed", "host2.local", []string{"host", "2", ".", "local"}},
		{"empty", "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitIntoSegments(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("splitIntoSegments(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseNumber(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantNum int
		wantOK  bool
	}{
		{"valid digits", "123", 123, true},
		{"single digit", "0", 0, true},
		{"empty string", "", 0, false},
		{"non-digit chars", "12a", 0, false},
		{"alpha only", "abc", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			num, ok := parseNumber(tt.input)
			if num != tt.wantNum || ok != tt.wantOK {
				t.Errorf("parseNumber(%q) = (%d, %v), want (%d, %v)", tt.input, num, ok, tt.wantNum, tt.wantOK)
			}
		})
	}
}

func TestIsWatchedDBFile(t *testing.T) {
	targets := map[string]struct{}{
		"/home/user/.config/weft/jobs.db":     {},
		"/home/user/.config/weft/jobs.db-wal": {},
		"/home/user/.config/weft/jobs.db-shm": {},
	}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"exact match", "/home/user/.config/weft/jobs.db", true},
		{"wal file", "/home/user/.config/weft/jobs.db-wal", true},
		{"clean path", "/home/user/.config/weft/../weft/jobs.db", true},
		{"empty name", "", false},
		{"missing key", "/home/user/.config/weft/other.db", false},
		{"partial match", "/home/user/.config/weft/jobs.d", false},
	}

	cleanTargets := make(map[string]struct{})
	for k, v := range targets {
		cleanTargets[filepath.Clean(k)] = v
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isWatchedDBFile(tt.path, cleanTargets)
			if got != tt.want {
				t.Errorf("isWatchedDBFile(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
