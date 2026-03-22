package campaign

import (
	"strings"

	"github.com/osteele/weft/internal/placement"
)

// gpuClassSupremum returns the most specific GPU class constraint that is
// compatible with both a and b (uppercase-normalized), or ok=false if
// incompatible.
//
// Empty string is treated as unconstrained (compatible with any GPU class).
// When one constraint subsumes the other, the narrower (more specific) one
// is returned.
func gpuClassSupremum(a, b string) (merged string, ok bool) {
	aNorm := strings.ToUpper(strings.TrimSpace(a))
	bNorm := strings.ToUpper(strings.TrimSpace(b))

	if aNorm == "" && bNorm == "" {
		return "", true
	}
	if aNorm == "" {
		return bNorm, true
	}
	if bNorm == "" {
		return aNorm, true
	}
	if aNorm == bNorm {
		return aNorm, true
	}

	ca := placement.ParseGPUConstraint(a)
	cb := placement.ParseGPUConstraint(b)

	if ca.Subsumes(cb) {
		return bNorm, true
	}
	if cb.Subsumes(ca) {
		return aNorm, true
	}

	return "", false
}
