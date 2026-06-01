package dataloc

import (
	"reflect"
	"testing"
)

func TestScanUVRunWith(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    []DepSpec
	}{
		{
			name:    "double-quoted with operator",
			command: `uv run --with "vllm>=0.17" python foo.py`,
			want:    []DepSpec{{Name: "vllm", Spec: ">=0.17"}},
		},
		{
			name:    "multiple --with",
			command: `uv run --with "vllm>=0.17" --with "pynvml>=12.0" python foo.py`,
			want: []DepSpec{
				{Name: "vllm", Spec: ">=0.17"},
				{Name: "pynvml", Spec: ">=12.0"},
			},
		},
		{
			name:    "unquoted equals form",
			command: `uv run --with=vllm==0.17.0 python foo.py`,
			want:    []DepSpec{{Name: "vllm", Spec: "==0.17.0"}},
		},
		{
			name:    "no operator (bare name)",
			command: `uv run --with numpy python foo.py`,
			want:    []DepSpec{{Name: "numpy", Spec: ""}},
		},
		{
			name:    "with-requirements is skipped",
			command: `uv run --with-requirements reqs.txt python foo.py`,
			want:    nil,
		},
		{
			name:    "no --with",
			command: `python foo.py`,
			want:    nil,
		},
		{
			name:    "case-insensitive name lowercased",
			command: `uv run --with "VLLM>=0.17" python foo.py`,
			want:    []DepSpec{{Name: "vllm", Spec: ">=0.17"}},
		},
		{
			name:    "extras stripped",
			command: `uv run --with "vllm[server]>=0.17" python foo.py`,
			want:    []DepSpec{{Name: "vllm", Spec: ">=0.17"}},
		},
		{
			name:    "comma-separated specs in a single --with",
			command: `uv run --with "vllm>=0.17,pynvml>=12.0" python foo.py`,
			want: []DepSpec{
				{Name: "vllm", Spec: ">=0.17"},
				{Name: "pynvml", Spec: ">=12.0"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanUVRunWith(tc.command)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ScanUVRunWith(%q) =\n  %+v\nwant\n  %+v", tc.command, got, tc.want)
			}
		})
	}
}

func TestParseDepSpec(t *testing.T) {
	cases := []struct {
		in   string
		want DepSpec
	}{
		{`"vllm>=0.17"`, DepSpec{Name: "vllm", Spec: ">=0.17"}},
		{`vllm ; python_version >= "3.10"`, DepSpec{Name: "vllm", Spec: ""}},
		{`torch==2.6.0+cu128`, DepSpec{Name: "torch", Spec: "==2.6.0+cu128"}},
		{`  `, DepSpec{}},
		{``, DepSpec{}},
	}
	for _, tc := range cases {
		got := parseDepSpec(tc.in)
		if got != tc.want {
			t.Errorf("parseDepSpec(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestSpecAdmitsMinVersion(t *testing.T) {
	cases := []struct {
		spec       string
		minVersion string
		want       bool
	}{
		{">=0.17", "0.17.0", true},
		{">=0.17.0", "0.17.0", true},
		{">=0.16", "0.17.0", true},  // ≥0.16 admits 0.17.x
		{"<0.17", "0.17.0", false},  // <0.17 excludes 0.17.x
		{"<=0.17", "0.17.0", true},  // ≤0.17 admits 0.17
		{"<=0.16", "0.17.0", false}, // ≤0.16 excludes 0.17
		{"==0.17.0", "0.17.0", true},
		{"==0.16.0", "0.17.0", false},
		{"==0.17", "0.17.0", true}, // 0.17 ≡ 0.17.0 for our purposes
		{"!=0.17", "0.17.0", true}, // != excludes one version; others still admit
		{"~=0.17", "0.17.0", true}, // compatible release matches 0.17.x
		{"~=0.16", "0.17.0", true}, // ~= admitted unconditionally; over-applies rather than under-applies
		{"", "0.17.0", true},       // unpinned → assume could resolve to latest
		{">=0.16,<0.18", "0.17.0", true},
		{">=0.16,<0.17", "0.17.0", false},
	}
	for _, tc := range cases {
		got := specAdmitsMinVersion(tc.spec, tc.minVersion)
		if got != tc.want {
			t.Errorf("specAdmitsMinVersion(%q, %q) = %v, want %v", tc.spec, tc.minVersion, got, tc.want)
		}
	}
}

func TestLibraryMinCUDAFromDeps(t *testing.T) {
	cases := []struct {
		name string
		deps []DepSpec
		want string
	}{
		{
			name: "vllm 0.17 triggers 12.8",
			deps: []DepSpec{{Name: "vllm", Spec: ">=0.17"}},
			want: "12.8",
		},
		{
			name: "vllm pinned below 0.17 does not trigger",
			deps: []DepSpec{{Name: "vllm", Spec: "<0.17"}},
			want: "",
		},
		{
			name: "vllm ==0.16 does not trigger",
			deps: []DepSpec{{Name: "vllm", Spec: "==0.16"}},
			want: "",
		},
		{
			name: "bare vllm name is conservatively treated as latest",
			deps: []DepSpec{{Name: "vllm", Spec: ""}},
			want: "12.8",
		},
		{
			name: "unknown library returns empty",
			deps: []DepSpec{{Name: "numpy", Spec: ">=1.0"}},
			want: "",
		},
		{
			name: "empty input returns empty",
			deps: nil,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LibraryMinCUDAFromDeps(tc.deps); got != tc.want {
				t.Errorf("LibraryMinCUDAFromDeps(%+v) = %q, want %q", tc.deps, got, tc.want)
			}
		})
	}
}
