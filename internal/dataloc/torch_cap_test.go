package dataloc

import "testing"

func TestResolveTorchMaxComputeCapForPersistence(t *testing.T) {
	cases := []struct {
		archMax string
		want    string
	}{
		{"any", "any"},
		{"hopper", "9.0"},
		{"9.0", "9.0"},
		{"", "any"},
	}
	for _, c := range cases {
		got := ResolveTorchMaxComputeCapForPersistence(c.archMax, "")
		if got != c.want {
			t.Errorf("ResolveTorchMaxComputeCapForPersistence(%q, \"\") = %q, want %q", c.archMax, got, c.want)
		}
	}
}
