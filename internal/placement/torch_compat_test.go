package placement

import "testing"

func TestTorchMaxComputeCap(t *testing.T) {
	cases := []struct {
		version string
		cuda    string
		want    string
	}{
		// Hopper-only era
		{"1.13.1", "cu117", "8.0"},
		{"2.0.1", "cu118", "9.0"},
		{"2.3.1", "cu121", "9.0"},
		{"2.4.1", "cu121", "9.0"},
		// 2.5: cu124 remains Hopper-bound; cu126 admits sm_100.
		{"2.5.0", "cu118", "9.0"},
		{"2.5.0", "cu121", "9.0"},
		{"2.5.1", "cu124", "9.0"},
		{"2.5.1", "cu126", "10.0"},
		// 2.6: sm_120 added on cu128
		{"2.6.0", "cu124", "9.0"},
		{"2.6.0", "cu126", "10.0"},
		{"2.6.0", "cu128", "12.0"},
		// 2.7+
		{"2.7.0", "cu126", "12.0"},
		{"2.7.0", "cu128", "12.0"},
		// CPU-only or unknown variant
		{"2.4.0", "cpu", ""},
		{"2.4.0", "", "9.0"},
		{"2.6.0", "", "9.0"},
		{"2.7.0", "", "12.0"},
		// Malformed version
		{"", "cu121", ""},
		{"abc", "cu121", ""},
		// Local-version suffix is stripped
		{"2.6.0+cu128", "cu128", "12.0"},
	}
	for _, c := range cases {
		got := TorchMaxComputeCap(c.version, c.cuda)
		if got != c.want {
			t.Errorf("TorchMaxComputeCap(%q, %q) = %q, want %q", c.version, c.cuda, got, c.want)
		}
	}
}

func TestTorchMinComputeCap(t *testing.T) {
	cases := []struct {
		version string
		cuda    string
		want    string
	}{
		{"2.7.0", "cu126", ""},
		{"2.7.0", "cu128", "7.5"},
		{"2.8.0", "", "7.5"},
		{"2.8.0", "cpu", ""},
		{"", "cu128", ""},
	}
	for _, c := range cases {
		got := TorchMinComputeCap(c.version, c.cuda)
		if got != c.want {
			t.Errorf("TorchMinComputeCap(%q, %q) = %q, want %q", c.version, c.cuda, got, c.want)
		}
	}
}

func TestComputeCapForGPU(t *testing.T) {
	cases := []struct {
		gpu  string
		want string
	}{
		{"A100", "8.0"},
		{"NVIDIA A100-PCIE-80GB", "8.0"},
		{"H100", "9.0"},
		{"RTX 3090", "8.6"},
		{"RTX 4090", "8.9"},
		{"B200", "10.0"},
		{"NVIDIA B200", "10.0"},
		{"RTX PRO 4500 Blackwell", "12.0"},
		{"RTX PRO 6000 WS", "12.0"},
		{"RTX 5090", "12.0"},
		{"unknown-gpu-name", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := ComputeCapForGPU(c.gpu)
		if got != c.want {
			t.Errorf("ComputeCapForGPU(%q) = %q, want %q", c.gpu, got, c.want)
		}
	}
}

func TestCompareComputeCap(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"9.0", "10.0", -1},
		{"10.0", "12.0", -1},
		{"12.0", "9.0", 1},
		{"9.0", "9.0", 0},
		{"", "9.0", -1},
		{"9.0", "", 1},
		{"", "", 0},
		{"garbage", "9.0", -1},
	}
	for _, c := range cases {
		got := CompareComputeCap(c.a, c.b)
		if got != c.want {
			t.Errorf("CompareComputeCap(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestArchNameToMaxCap(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"ampere", "8.6"},
		{"hopper", "9.0"},
		{"blackwell", "12.0"},
		{"any", ""},
		{"", ""},
		{"unknown-arch", ""},
		{"AMPERE", "8.6"},
	}
	for _, c := range cases {
		got := ArchNameToMaxCap(c.name)
		if got != c.want {
			t.Errorf("ArchNameToMaxCap(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMaxComputeCapForJob_Override(t *testing.T) {
	cases := []struct {
		archMax string
		want    string
	}{
		{"any", ""},
		{"hopper", "9.0"},
		{"9.0", "9.0"},
		{"12.0", "12.0"},
		{"unknown", ""},
	}
	for _, c := range cases {
		got := MaxComputeCapForJob(c.archMax, "")
		if got != c.want {
			t.Errorf("MaxComputeCapForJob(%q, \"\") = %q, want %q", c.archMax, got, c.want)
		}
	}
}

func TestResolveMaxComputeCapForPersistence(t *testing.T) {
	// dir="" means ScanTorchPin returns nil — represents a project with no
	// readable torch pin (pure CPU, missing source, etc.). The persisted-cap
	// resolver should record "any" rather than "" so downstream readers can
	// distinguish "explicitly unbounded" from "unresolved".
	cases := []struct {
		archMax string
		want    string
	}{
		{"any", "any"},
		{"hopper", "9.0"},
		{"9.0", "9.0"},
		{"", "any"}, // no override + no torch pin → explicit unbounded
	}
	for _, c := range cases {
		got := ResolveMaxComputeCapForPersistence(c.archMax, "")
		if got != c.want {
			t.Errorf("ResolveMaxComputeCapForPersistence(%q, \"\") = %q, want %q", c.archMax, got, c.want)
		}
	}
}
