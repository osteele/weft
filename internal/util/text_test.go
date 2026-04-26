package util

import "testing"

func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"empty-zero-max", "abc", 0, ""},
		{"negative-max", "abc", -1, ""},
		{"under-limit", "abc", 10, "abc"},
		{"exact-limit", "abc", 3, "abc"},
		{"max-too-small-for-marker", "abcdef", 1, "a"},
		{"max-equals-marker-bytes", "abcdef", 3, "abc"},
		{"truncates-with-marker", "abcdefghij", 6, "abc" + Ellipsis},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Truncate(c.in, c.max)
			if got != c.want {
				t.Fatalf("Truncate(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
			}
		})
	}
}
