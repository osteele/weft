package compat

import (
	"strings"
	"testing"
)

func TestCheckVersionMin(t *testing.T) {
	reqs := []Requirement{{
		Axis:          AxisCUDA,
		Comparator:    ComparatorVersionMin,
		Value:         "12.4",
		MissingPolicy: MissingFailClosed,
	}}
	if got := Check(reqs, FactSet{AxisCUDA: "12.4"}); len(got) != 0 {
		t.Fatalf("equal version violated: %+v", got)
	}
	violations := Check(reqs, FactSet{AxisCUDA: "12.0"})
	if len(violations) != 1 || violations[0].Actual != "12.0" || violations[0].Required != "12.4" {
		t.Fatalf("violations = %+v", violations)
	}
}

func TestCheckMissingPolicies(t *testing.T) {
	open := []Requirement{{
		Axis:          AxisGLIBCXX,
		Comparator:    ComparatorVersionMin,
		Value:         "3.4.30",
		MissingPolicy: MissingFailOpen,
	}}
	if got := Check(open, FactSet{}); len(got) != 0 {
		t.Fatalf("fail-open missing fact violated: %+v", got)
	}
	closed := []Requirement{{
		Axis:          AxisCUDA,
		Comparator:    ComparatorVersionMin,
		Value:         "12.8",
		MissingPolicy: MissingFailClosed,
	}}
	if got := Check(closed, FactSet{}); len(got) != 1 {
		t.Fatalf("fail-closed missing fact violations = %+v", got)
	}
}

func TestFormatViolation(t *testing.T) {
	tests := []struct {
		name string
		v    Violation
		want string
	}{
		{
			name: "driver missing",
			v:    Violation{Axis: AxisNVIDIADriver, Required: "570"},
			want: "driver floor: no recorded NVIDIA driver, require >=570",
		},
		{
			name: "driver old",
			v:    Violation{Axis: AxisNVIDIADriver, Actual: "550.120", Required: "570"},
			want: "driver floor: NVIDIA driver 550.120 < required >=570",
		},
		{
			name: "cuda missing",
			v:    Violation{Axis: AxisCUDA, Required: "12.8"},
			want: "CUDA floor: no recorded CUDA compatibility, require >=12.8",
		},
		{
			name: "cuda old",
			v:    Violation{Axis: AxisCUDA, Actual: "12.4", Required: "12.8"},
			want: "CUDA floor: host CUDA 12.4 < required 12.8",
		},
		{
			name: "glibcxx old",
			v:    Violation{Axis: AxisGLIBCXX, Actual: "3.4.28", Required: "3.4.30", Origin: "pysr"},
			want: "GLIBCXX floor: host libstdc++ 3.4.28 < required 3.4.30, from pysr",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatViolation(tt.v); !strings.Contains(got, tt.want) {
				t.Fatalf("FormatViolation = %q, want %q", got, tt.want)
			}
		})
	}
}
