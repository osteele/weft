package edge

import (
	"fmt"
	"time"
)

// PayloadKind names the schema of a submission's payload, e.g.
// "weft.job-submission/v1".
//
// The kind is an explicit discriminator so the hub routes to a validator
// rather than inferring one from the payload's shape. A reader that guesses
// its schema is the failure the protocol's version refusal exists to prevent,
// and guessing is worse here because a second caller's payload is a different
// shape entirely.
type PayloadKind string

const (
	// KindWeftJobSubmission is a weft job submission.
	KindWeftJobSubmission PayloadKind = "weft.job-submission/v1"
	// KindWeftJobControl asks the hub to act on an existing job.
	KindWeftJobControl PayloadKind = "weft.job-control/v1"
	// KindWeftBugReport records a bug report or note on the hub.
	KindWeftBugReport PayloadKind = "weft.bug-report/v1"
	// KindPlanEnded asks the hub to close a plan's spend authority.
	//
	// It is the one thing an edge may assert about its own authority, because
	// it can only ever reduce it. The rule that an edge must not assert
	// liveness exists so a compromised edge cannot extend its own window; a
	// compromised edge ending its own plan defeats nothing.
	KindPlanEnded PayloadKind = "weft.plan-ended/v1"
)

// KindPolicy is the per-kind configuration the hub applies.
type KindPolicy struct {
	// TTL bounds how stale a submission of this kind may be. It is per-kind
	// because staleness means different things: a job submission admitted six
	// hours late may be unwanted work, while a record that states a fact is
	// still true six hours later.
	//
	// Zero means "use the hub's default", not "never expire". A kind that
	// genuinely should not expire must say so with NeverExpires, so that a
	// forgotten TTL fails toward the default rather than toward no bound.
	TTL time.Duration
	// NeverExpires exempts a kind from staleness entirely.
	//
	// This exists for kinds whose payload states a fact rather than requesting
	// work: such a record does not become false by arriving late, and dropping
	// it would be read downstream as the event never having happened — a
	// silent absence standing in for a negative observation.
	NeverExpires bool
	// ContentIdempotent declares that two submissions with identical payloads
	// are the same submission, whatever their nonces.
	//
	// This is false for job submissions: running the same command twice is a
	// legitimate thing to ask for, so two nonces must mean two jobs. It is
	// true for kinds whose payload *is* their identity, where a resubmission
	// should collapse rather than duplicate.
	ContentIdempotent bool
}

// KindRegistry maps payload kinds to their policies. A kind absent from the
// registry is refused rather than admitted under a default, so adding a caller
// is a deliberate act on the hub.
type KindRegistry struct {
	kinds map[PayloadKind]KindPolicy
}

func NewKindRegistry() *KindRegistry {
	return &KindRegistry{kinds: map[PayloadKind]KindPolicy{}}
}

// DefaultKindRegistry is the registry a weft hub uses out of the box.
func DefaultKindRegistry() *KindRegistry {
	r := NewKindRegistry()
	r.Register(KindWeftJobSubmission, KindPolicy{
		TTL:               time.Hour,
		ContentIdempotent: false,
	})
	// Control requests concern work in flight. Fifteen minutes accommodates
	// ordinary poll delay and transient store trouble without letting an old
	// cancel, edit, or restart surprise a job after its context has changed.
	// RequestID makes the content identify one instruction, so retrying that
	// instruction collapses while two deliberate instructions remain distinct.
	r.Register(KindWeftJobControl, KindPolicy{
		TTL:               15 * time.Minute,
		ContentIdempotent: true,
	})
	// A bug report states a fact. It remains true however late the hub receives
	// it, and dropping it would turn missing evidence into an apparent negative.
	// ReportID makes each report or note identity-bearing for content deduping.
	r.Register(KindWeftBugReport, KindPolicy{
		NeverExpires:      true,
		ContentIdempotent: true,
	})
	// Ending a plan twice is ending it once, so this kind dedupes on content.
	// The TTL is generous because a late-arriving termination is still correct:
	// closing an authority that is already closed is harmless, and refusing a
	// stale one would leave a grant open.
	r.Register(KindPlanEnded, KindPolicy{
		TTL:               24 * time.Hour,
		ContentIdempotent: true,
	})
	return r
}

func (r *KindRegistry) Register(kind PayloadKind, policy KindPolicy) {
	r.kinds[kind] = policy
}

func (r *KindRegistry) Lookup(kind PayloadKind) (KindPolicy, bool) {
	if r == nil {
		return KindPolicy{}, false
	}
	p, ok := r.kinds[kind]
	return p, ok
}

func (r *KindRegistry) Known() []PayloadKind {
	if r == nil {
		return nil
	}
	out := make([]PayloadKind, 0, len(r.kinds))
	for k := range r.kinds {
		out = append(out, k)
	}
	return out
}

func (r *KindRegistry) String() string {
	return fmt.Sprintf("%v", r.Known())
}
