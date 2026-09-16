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

func TestResolveTorchMaxComputeCapForPersistence_Torch26Cu128(t *testing.T) {
	dir := t.TempDir()
	WriteTestTorchPin(t, dir, "2.6.0", "cu128")

	if got := ResolveTorchMaxComputeCapForPersistence("", dir); got != "9.0" {
		t.Fatalf("resolved cap = %q, want canonical sm_90 cap %q", got, "9.0")
	}
}
