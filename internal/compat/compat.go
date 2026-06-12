package compat

import (
	"fmt"
	"strconv"
	"strings"
)

type Axis string

const (
	AxisCUDA         Axis = "cuda"
	AxisNVIDIADriver Axis = "nvidia_driver"
	AxisGLIBCXX      Axis = "glibcxx"
)

type Comparator string

const ComparatorVersionMin Comparator = "version_min"

type MissingPolicy string

const (
	MissingFailOpen   MissingPolicy = "fail_open"
	MissingFailClosed MissingPolicy = "fail_closed"
)

type Requirement struct {
	Axis          Axis
	Comparator    Comparator
	Value         string
	Origin        string
	MissingPolicy MissingPolicy
}

type FactSet map[Axis]string

type Violation struct {
	Axis          Axis
	Required      string
	Actual        string
	Origin        string
	MissingPolicy MissingPolicy
}

func Check(requirements []Requirement, facts FactSet) []Violation {
	var violations []Violation
	for _, req := range requirements {
		if strings.TrimSpace(req.Value) == "" {
			continue
		}
		actual := strings.TrimSpace(facts[req.Axis])
		if actual == "" {
			if req.MissingPolicy == MissingFailClosed {
				violations = append(violations, Violation{
					Axis:          req.Axis,
					Required:      req.Value,
					Origin:        req.Origin,
					MissingPolicy: req.MissingPolicy,
				})
			}
			continue
		}
		if req.Comparator == ComparatorVersionMin && compareDottedVersion(actual, req.Value) < 0 {
			violations = append(violations, Violation{
				Axis:          req.Axis,
				Required:      req.Value,
				Actual:        actual,
				Origin:        req.Origin,
				MissingPolicy: req.MissingPolicy,
			})
		}
	}
	return violations
}

func FormatViolation(v Violation) string {
	switch v.Axis {
	case AxisNVIDIADriver:
		if strings.TrimSpace(v.Actual) == "" {
			return fmt.Sprintf("driver floor: no recorded NVIDIA driver, require >=%s", v.Required)
		}
		return fmt.Sprintf("driver floor: NVIDIA driver %s < required >=%s", v.Actual, v.Required)
	case AxisCUDA:
		if strings.TrimSpace(v.Actual) == "" {
			return fmt.Sprintf("CUDA floor: no recorded CUDA compatibility, require >=%s", v.Required)
		}
		return fmt.Sprintf("CUDA floor: host CUDA %s < required %s", v.Actual, v.Required)
	case AxisGLIBCXX:
		origin := ""
		if v.Origin != "" {
			origin = fmt.Sprintf(", from %s", v.Origin)
		}
		return fmt.Sprintf("GLIBCXX floor: host libstdc++ %s < required %s%s", v.Actual, v.Required, origin)
	default:
		if strings.TrimSpace(v.Actual) == "" {
			return fmt.Sprintf("%s floor: no recorded value, require >=%s", v.Axis, v.Required)
		}
		return fmt.Sprintf("%s floor: %s < required %s", v.Axis, v.Actual, v.Required)
	}
}

func compareDottedVersion(a, b string) int {
	as := splitVersion(a)
	bs := splitVersion(b)
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

func splitVersion(v string) []int {
	var out []int
	for _, part := range strings.Split(strings.TrimSpace(v), ".") {
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}
