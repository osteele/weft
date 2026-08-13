package blockreason

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/db"
)

// ReuseRejection records why one running instance refused to accept a job.
//
// Reason is the compact, sanitized one-liner used for collapsed-row display.
// Detail is the optional full, untruncated reason (preserved newlines and all)
// for expand views and CLI surfaces. When Detail is empty, callers should fall
// back to Reason.
type ReuseRejection struct {
	Instance string `json:"instance"`         // display instance id, e.g. "wi3022"
	Reason   string `json:"reason"`           // compact, sanitized
	Detail   string `json:"detail,omitempty"` // full, untruncated when distinct from Reason
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
//
// Summary/Launch carry compact, sanitized one-liners suitable for collapsed
// rows and column-width display. LaunchDetail (and ReuseRejection.Detail)
// carry the optional full, untruncated reason (preserved newlines and all)
// used by the TUI expand view and CLI diagnose/show surfaces. When the detail
// fields are empty, callers should fall back to the sanitized fields.
//
// Fingerprint is an optional stable coalescing key (shape:
// "<provider>/<op>/<class>[:<key>]" — see cloud.ProviderError). When set,
// display surfaces SHOULD group blocked jobs sharing the same Fingerprint
// into a single incident row instead of bucketing per filter-prefix
// variation. Empty when no upstream classification was available; absence
// reverts to today's per-message bucketing.
type Structured struct {
	Summary      string           `json:"summary"`
	Launch       string           `json:"launch,omitempty"`
	LaunchDetail string           `json:"launch_detail,omitempty"`
	Fingerprint  string           `json:"fingerprint,omitempty"`
	Reuse        []ReuseRejection `json:"reuse,omitempty"`
	// OnPrem records why every on-prem inventory host rejected the job
	// (per-host rejection detail from the placement scorer). Persisting it
	// here lets weft info/explain show the on-prem avenue after a daemon
	// restart — previously it lived only in the oplog.
	OnPrem string `json:"onprem,omitempty"`
	// LastAttempt carries optional positive-evidence context about the most
	// recent instance. Empty when no caller has authoritative attempt context.
	LastAttempt string `json:"last_attempt,omitempty"`
}

// IsPlacementFailure reports whether the reason has the launch/reuse structure
// that warrants an expandable per-avenue breakdown. It is false for
// single-cause blockers, whose Summary already tells the whole story.
func (s *Structured) IsPlacementFailure() bool {
	if s == nil {
		return false
	}
	return strings.TrimSpace(s.Launch) != "" || len(s.Reuse) > 0 || strings.TrimSpace(s.OnPrem) != "" || strings.TrimSpace(s.LastAttempt) != ""
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

// MergeRecordedReuse overlays recorded reuse outcomes — actual match/submit
// failures observed during the pass — onto the re-probed rejections. A
// recorded entry replaces the probe's entry for the same instance and is
// otherwise prepended: recorded outcomes reflect what actually happened,
// while a post-hoc probe can disagree (e.g. the match reports compatible
// after the submit already failed) and would hide the real failure.
func (s *Structured) MergeRecordedReuse(recorded []ReuseRejection) {
	if s == nil || len(recorded) == 0 {
		return
	}
	byInstance := map[string]int{}
	for i, r := range s.Reuse {
		byInstance[r.Instance] = i
	}
	var prepend []ReuseRejection
	for _, r := range recorded {
		if i, ok := byInstance[r.Instance]; ok && r.Instance != "" {
			s.Reuse[i] = r
		} else {
			prepend = append(prepend, r)
		}
	}
	s.Reuse = append(prepend, s.Reuse...)
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
	return "reuse: " + first + " + " + Plural(len(s.Reuse)-1, "more instance", "more instances")
}

// DetailLines returns the expanded per-avenue breakdown for an in-place TUI
// disclosure. The launch avenue is listed first, then each running instance.
// When LaunchDetail (or ReuseRejection.Detail) is set, its full, untruncated
// text is emitted with multi-line content split across rows so the user can
// read the underlying provider stderr instead of a clipped one-liner.
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
	lines := make([]string, 0, 3+len(s.Reuse))
	if launch := strings.TrimSpace(s.Launch); launch != "" {
		lines = appendAvenueLines(lines, "new instance", launch, s.LaunchDetail)
	}
	if last := strings.TrimSpace(s.LastAttempt); last != "" {
		lines = appendAvenueLines(lines, "last attempt", last, "")
	}
	if onPrem := strings.TrimSpace(s.OnPrem); onPrem != "" {
		lines = appendAvenueLines(lines, "on-prem hosts", onPrem, "")
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
		lines = appendAvenueLines(lines, "reuse "+inst, reason, r.Detail)
	}
	return lines
}

// appendAvenueLines emits one disclosure row for an avenue, expanding the
// optional full Detail across multiple rows when it carries more information
// than the compact reason. Subsequent rows are indented under the avenue label
// so the structure stays visible.
func appendAvenueLines(lines []string, label, compact, detail string) []string {
	lines = append(lines, label+"  "+compact)
	detail = strings.TrimSpace(detail)
	if detail == "" || detail == compact {
		return lines
	}
	// Only emit extra rows when the detail adds information beyond the
	// compact line. Single-line details that are a prefix of the compact form
	// are redundant.
	if !strings.ContainsRune(detail, '\n') && strings.HasPrefix(compact, detail) {
		return lines
	}
	indent := strings.Repeat(" ", len(label)+2)
	for _, ln := range strings.Split(detail, "\n") {
		ln = strings.TrimRight(ln, " \t\r")
		if ln == "" {
			continue
		}
		lines = append(lines, indent+ln)
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

// Plural renders a count with its noun ("1 job", "3 jobs"). It is the shared
// helper for blocker/incident humanization; display code uses it rather than
// keeping its own copy.
func Plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(n) + " " + pluralForm
}
