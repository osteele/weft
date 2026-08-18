package blockreason

import "strings"

// BlockerKind classifies an authoritative launch blocker by origin. The
// recheck class of a blocker is a property of its kind, so the pass that
// records the blocker decides the wake cadence without re-parsing the reason
// string.
type BlockerKind int

const (
	// KindUnclassified is the safety net for blockers recorded as raw
	// external strings nobody typed. Its recheck class is derived from the
	// reason string via Recheck, preserving the string-path boundary
	// classifier for everything not produced by a typed write site.
	KindUnclassified BlockerKind = iota

	// KindMarket is an offer/launch failure: only a fresh provider query
	// resolves it.
	KindMarket

	// KindBudget is the run-rate gate verdict: a launch ending or an edited
	// hourly target, both local writes, clears it.
	KindBudget

	// KindBackoff is a retry cooldown/deadline that passes on a clock with no
	// accompanying write.
	KindBackoff

	// KindPrecondition is a database-observable gate: dependencies, checkpoint
	// publish pending, inventory awaiting host, paused.
	KindPrecondition
)

// RecheckNeed returns the recheck class implied by the kind. The bool is
// false for KindUnclassified, whose class must come from the reason string
// via Recheck.
func (k BlockerKind) RecheckNeed() (RecheckNeed, bool) {
	switch k {
	case KindMarket:
		return RecheckMarket, true
	case KindBudget, KindPrecondition:
		return RecheckNone, true
	case KindBackoff:
		return RecheckDeadline, true
	default:
		return RecheckNone, false
	}
}

// String names the kind for logs and tests.
func (k BlockerKind) String() string {
	switch k {
	case KindMarket:
		return "market"
	case KindBudget:
		return "budget"
	case KindBackoff:
		return "backoff"
	case KindPrecondition:
		return "precondition"
	case KindUnclassified:
		return "unclassified"
	default:
		return "unknown"
	}
}

// ProbeFunc re-probes a job's placement avenues and returns the structured
// launch/reuse breakdown. It is injected by the autopilot pass because it
// needs a database (placementFailureStructured in orchestration); blockreason
// itself stays free of DB and orchestration imports.
type ProbeFunc func(launch string) *Structured

// VerdictBuilder accumulates the unsettled blocked-reason state for one job:
// the authoritative launch blocker (first one recorded wins), the
// opportunistic reuse diagnostics, the recorded per-instance reuse outcomes,
// the on-prem rejection detail, and any planner-supplied structured
// breakdown. Settle produces the final (flat, *Structured) pair, encoding the
// precedence that used to be spread across the four reattach loops in
// orchestration's finalizeUnplacedBlockedReasons.
//
// Structured is the settled primary+secondary form persisted to
// placement_blocked; this builder is the unsettled form that produces it.
type VerdictBuilder struct {
	kind              BlockerKind
	launchBlocker     string
	secondary         []secondaryBlocker
	diags             []string
	recordedReuse     []ReuseRejection
	onPremDetail      string
	plannerStructured *Structured
}

// secondaryBlocker is an authoritative gate discovered after the primary
// blocker was already recorded (e.g. a job dependency found during reuse fill
// on a job the planner had blocked). It joins the flat reason as an
// additional fragment rather than displacing the primary.
type secondaryBlocker struct {
	kind   BlockerKind
	reason string
}

// SetLaunchBlocker records the authoritative launch-path reason. The first
// recorded reason wins, preserving the pass's "set only if absent" discipline
// at every write site.
func (vb *VerdictBuilder) SetLaunchBlocker(kind BlockerKind, reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" || vb.launchBlocker != "" {
		return
	}
	vb.kind = kind
	vb.launchBlocker = reason
}

// AddBlocker records an authoritative blocker without displacing an earlier
// one: the first stays primary and later ones join the flat reason as
// additional fragments, deduplicated by containment against the joined base.
func (vb *VerdictBuilder) AddBlocker(kind BlockerKind, reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return
	}
	if vb.launchBlocker == "" {
		vb.kind = kind
		vb.launchBlocker = reason
		return
	}
	base := vb.BaseReason()
	if base == reason || strings.Contains(base, reason) {
		return
	}
	vb.secondary = append(vb.secondary, secondaryBlocker{kind: kind, reason: reason})
}

// ReplaceLaunchBlocker overwrites the whole authoritative verdict with a
// fresher one (e.g. the reuse-preferred retry planner re-blocking a job its
// first plan had already blocked). Earlier secondary fragments go with the
// blocker they annotated; reuse diagnostics are kept.
func (vb *VerdictBuilder) ReplaceLaunchBlocker(kind BlockerKind, reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return
	}
	vb.kind = kind
	vb.launchBlocker = reason
	vb.secondary = nil
}

// AddReuseDiagnostic records one opportunistic reuse-rejection note. The
// diagnostics are appended as trailing detail at settle time and never become
// the primary reason. Duplicate fragments are deduplicated with the same
// containment rule the pass's old addAutoPilotBlockedReason used.
func (vb *VerdictBuilder) AddReuseDiagnostic(diag string) {
	diag = strings.TrimSpace(diag)
	if diag == "" {
		return
	}
	joined := strings.Join(vb.diags, "; ")
	if joined == diag || strings.Contains(joined, diag) {
		return
	}
	vb.diags = append(vb.diags, diag)
}

// RecordReuse appends recorded per-instance reuse outcomes (actual
// match/submit failures observed during the pass).
func (vb *VerdictBuilder) RecordReuse(rejections []ReuseRejection) {
	if len(rejections) == 0 {
		return
	}
	vb.recordedReuse = append(vb.recordedReuse, rejections...)
}

// SetOnPremDetail records why every on-prem inventory host rejected the job.
func (vb *VerdictBuilder) SetOnPremDetail(detail string) {
	detail = strings.TrimSpace(detail)
	if detail == "" || vb.onPremDetail != "" {
		return
	}
	vb.onPremDetail = detail
}

// SetPlannerStructured attaches a structured breakdown that arrived from the
// planner with a prebuilt launch/reuse form. When present, settle uses it
// as-is instead of probing.
func (vb *VerdictBuilder) SetPlannerStructured(s *Structured) {
	if s == nil {
		return
	}
	vb.plannerStructured = s
}

// HasLaunchBlocker reports whether an authoritative launch reason has been
// recorded. The pass uses it for the mid-pass "is this job blocked" control
// flow that used to read the flat map directly.
func (vb *VerdictBuilder) HasLaunchBlocker() bool {
	return vb.launchBlocker != ""
}

// RecordedReuse returns the recorded per-instance reuse outcomes.
func (vb *VerdictBuilder) RecordedReuse() []ReuseRejection {
	return vb.recordedReuse
}

// Diagnostics returns the joined reuse-diagnostic fragment exactly as Settle
// would append it, for tests and logging. Empty when none were recorded.
func (vb *VerdictBuilder) Diagnostics() string {
	if len(vb.diags) == 0 {
		return ""
	}
	return strings.Join(vb.diags, "; ")
}

// Settle produces the final (flat string, *Structured) pair for the job. The
// flat string leads with the authoritative launch blocker and carries the
// reuse diagnostics as trailing detail; a builder with no launch blocker
// settles to empty. The probe is called only when a structured form is
// missing and a rule needs one.
//
// The precedence, in order:
//
//   - a planner-supplied structured form wins outright;
//   - a no-rental-headroom blocker without a structured form gets a probed
//     one when the probe is a placement failure;
//   - a run-rate budget blocker without a structured form gets a probed one
//     only when the probe yields at least one reuse rejection (a
//     busy-but-compatible instance yields none, and the budget reason stays
//     the whole story);
//   - a blocker with observed reuse or on-prem detail gets a probed form with
//     Summary set to the flat reason;
//   - recorded reuse failures overlay the re-probed entries, and on-prem
//     detail rides along on Structured.OnPrem.
func (vb *VerdictBuilder) Settle(probe ProbeFunc) (string, *Structured) {
	flat := vb.flat()
	if flat == "" {
		return "", nil
	}
	s := vb.plannerStructured
	if s == nil && probe != nil {
		if strings.HasPrefix(flat, NoRentalHeadroomHeadline) {
			if probed := probe(NoRentalHeadroomHeadline); probed.IsPlacementFailure() {
				s = probed
			}
		}
		if s == nil && IsRunRateBudgetReason(flat) {
			if probed := probe(stripReuseDiagnosticsForProbe(flat)); len(probed.Reuse) > 0 {
				s = probed
			}
		}
		if s == nil && vb.hasReuseDetail() {
			probed := probe(stripReuseDiagnosticsForProbe(flat))
			probed.Summary = flat
			s = probed
		}
	}
	if s != nil {
		s.MergeRecordedReuse(vb.recordedReuse)
		if vb.onPremDetail != "" {
			s.OnPrem = vb.onPremDetail
		}
	}
	return flat, s
}

// Need returns the recheck class of the settled verdict: the strongest need
// among the primary and secondary blockers. For KindUnclassified fragments it
// falls back to classifying the reason string with Recheck, keeping the
// string-path boundary classifier for untyped reasons. A backoff countdown
// nested in a reuse diagnostic names a clock gating the reuse avenue even
// when the launch blocker itself is database-observable; the deadline passes
// silently, so only a timer discovers it — mirroring the whole-reason
// evaluation in Recheck.
func (vb *VerdictBuilder) Need() RecheckNeed {
	need, typed := vb.kind.RecheckNeed()
	if !typed {
		need = Recheck(vb.flat())
	}
	for _, sec := range vb.secondary {
		secNeed, secTyped := sec.kind.RecheckNeed()
		if !secTyped {
			secNeed = Recheck(sec.reason)
		}
		if secNeed > need {
			need = secNeed
		}
	}
	if need < RecheckDeadline && hasBackoffCountdown(vb.Diagnostics()) {
		need = RecheckDeadline
	}

	return need
}

func (vb *VerdictBuilder) hasReuseDetail() bool {
	return len(vb.diags) > 0 || len(vb.recordedReuse) > 0 || vb.onPremDetail != ""
}

// flat joins the launch blocker, the secondary blockers, and the reuse
// diagnostics exactly the way the old joining writes and finalize loop did:
// fragments append in recording order, deduplicated by containment, with the
// diagnostics joined into one trailing fragment.
func (vb *VerdictBuilder) flat() string {
	base := vb.BaseReason()
	if base == "" {
		return ""
	}
	if len(vb.diags) == 0 {
		return base
	}
	return appendReasonPart(base, strings.Join(vb.diags, "; "))
}

// BaseReason is the authoritative portion of the flat reason: the primary
// blocker with the secondary fragments joined on, before any diagnostics.
// The pass's live flat projection mirrors it.
func (vb *VerdictBuilder) BaseReason() string {
	if vb.launchBlocker == "" {
		return ""
	}
	base := vb.launchBlocker
	for _, sec := range vb.secondary {
		base = appendReasonPart(base, sec.reason)
	}
	return base
}

// appendReasonPart joins a fragment to a flat reason with the pass's old
// addAutoPilotBlockedReason semantics: an identical or contained fragment is
// not appended twice.
func appendReasonPart(existing, reason string) string {
	existing = strings.TrimSpace(existing)
	reason = strings.TrimSpace(reason)
	if existing == "" {
		return reason
	}
	if reason == "" {
		return existing
	}
	if existing == reason || strings.Contains(existing, reason) {
		return existing
	}
	return existing + "; " + reason
}

// stripReuseDiagnosticsForProbe removes reuse-diagnostic fragments from a
// flat reason so the launch portion can be re-probed without the diagnostic
// tail. A flat that becomes empty after stripping falls back to the whole
// reason.
func stripReuseDiagnosticsForProbe(flat string) string {
	launch := StripReuseDiagnostics(flat)
	if launch == "" {
		return flat
	}
	return launch
}
