package blockreason

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/db"
)

// ReuseRejection records why one running instance refused to accept a job.
type ReuseRejection struct {
	Instance string `json:"instance"` // display instance id, e.g. "wi3022"
	Reason   string `json:"reason"`
}

// Structured is the full explanation of why a job cannot be placed.
//
// A job is blocked only when every placement avenue fails: launching a new
// instance (Launch) and reusing each running instance (Reuse). Launch is the
// primary, authoritative reason; Reuse entries are secondary per-instance
// detail — this mirrors specs/campaign-lifecycle.allium §
// AutoPilotBlockedReasonIsAuthoritative.
//
// Single-cause blockers — an unmet precondition, a retry cooldown — leave
// Launch and Reuse empty and carry the whole explanation in Summary.
type Structured struct {
	Summary string           `json:"summary"`
	Launch  string           `json:"launch,omitempty"`
	Reuse   []ReuseRejection `json:"reuse,omitempty"`
}

// IsPlacementFailure reports whether the reason has the launch/reuse structure
// that warrants an expandable per-avenue breakdown. It is false for
// single-cause blockers, whose Summary already tells the whole story.
func (s *Structured) IsPlacementFailure() bool {
	if s == nil {
		return false
	}
	return strings.TrimSpace(s.Launch) != "" || len(s.Reuse) > 0
}

// Flat renders the structured reason as the one-line string used by display
// surfaces that have not adopted the structured form (narrate, job diff, the
// placement_reasons history column).
func (s *Structured) Flat() string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(s.Summary)
}

// ReuseHeadline is the compact reuse-side summary for a collapsed list row,
// shown once the shared launch blocker has been hoisted to the group header.
func (s *Structured) ReuseHeadline() string {
	if s == nil || len(s.Reuse) == 0 {
		return "no running instances to reuse"
	}
	first := strings.TrimSpace(s.Reuse[0].Reason)
	if first == "" {
		first = "incompatible"
	}
	if len(s.Reuse) == 1 {
		return "reuse: " + first
	}
	return "reuse: " + first + " + " + plural(len(s.Reuse)-1, "more instance", "more instances")
}

// DetailLines returns the expanded per-avenue breakdown, one line per
// placement avenue, for an in-place TUI disclosure. The launch avenue is
// listed first, then each running instance.
func (s *Structured) DetailLines() []string {
	if !s.IsPlacementFailure() {
		if s == nil {
			return nil
		}
		if summary := strings.TrimSpace(s.Summary); summary != "" {
			return []string{summary}
		}
		return nil
	}
	lines := make([]string, 0, 1+len(s.Reuse))
	if launch := strings.TrimSpace(s.Launch); launch != "" {
		lines = append(lines, "new instance  "+launch)
	}
	for _, r := range s.Reuse {
		inst := strings.TrimSpace(r.Instance)
		if inst == "" {
			inst = "instance"
		}
		reason := strings.TrimSpace(r.Reason)
		if reason == "" {
			reason = "incompatible"
		}
		lines = append(lines, "reuse "+inst+"  "+reason)
	}
	return lines
}

// Marshal encodes the reason as JSON for the jobs.placement_blocked column.
// A nil or empty reason marshals to the empty string.
func (s *Structured) Marshal() string {
	if s == nil || (strings.TrimSpace(s.Summary) == "" && !s.IsPlacementFailure()) {
		return ""
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// ForJob returns the persisted structured launch/reuse breakdown for a job,
// or nil when the job carries none (single-cause blocker, or not blocked).
func ForJob(job *db.Job) *Structured {
	if job == nil {
		return nil
	}
	return Parse(job.PlacementBlockedJSON)
}

// Parse decodes a jobs.placement_blocked column value. It returns nil for an
// empty or malformed value so callers fall back to the flat-string path.
func Parse(encoded string) *Structured {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil
	}
	var s Structured
	if err := json.Unmarshal([]byte(encoded), &s); err != nil {
		return nil
	}
	return &s
}

func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(n) + " " + pluralForm
}
