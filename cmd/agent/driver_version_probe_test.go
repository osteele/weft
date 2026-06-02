package main

import "testing"

func TestParseDriverMajor(t *testing.T) {
	cases := []struct {
		name        string
		out         string
		wantVersion string
		wantMajor   int
		wantOK      bool
	}{
		{
			name:        "single line",
			out:         "570.133.07\n",
			wantVersion: "570.133.07",
			wantMajor:   570,
			wantOK:      true,
		},
		{
			name:        "two-component version",
			out:         "525.105\n",
			wantVersion: "525.105",
			wantMajor:   525,
			wantOK:      true,
		},
		{
			name:        "multi-GPU host (uses first row)",
			out:         "570.133.07\n570.133.07\n570.133.07\n",
			wantVersion: "570.133.07",
			wantMajor:   570,
			wantOK:      true,
		},
		{
			name:        "leading whitespace and blank lines",
			out:         "\n  580.65.06  \n",
			wantVersion: "580.65.06",
			wantMajor:   580,
			wantOK:      true,
		},
		{
			name:   "no numeric content",
			out:    "Failed to initialize NVML: Unknown Error\n",
			wantOK: false,
		},
		{
			name:   "empty",
			out:    "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			version, major, ok := parseDriverMajor(tc.out)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if version != tc.wantVersion {
				t.Errorf("version = %q, want %q", version, tc.wantVersion)
			}
			if major != tc.wantMajor {
				t.Errorf("major = %d, want %d", major, tc.wantMajor)
			}
		})
	}
}
