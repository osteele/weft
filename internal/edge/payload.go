package edge

import (
	"encoding/json"
	"fmt"
	"time"
)

// WeftJobPayload is the payload for KindWeftJobSubmission.
//
// The authority fields live here rather than in the shared envelope. They are
// no less protected for it: the envelope's PayloadDigest is inside the
// signature, so altering this payload invalidates the pointer that names it. A
// party who can write the inbound store but cannot sign still cannot raise a
// ceiling or redirect execution.
//
// What the move buys is that a caller with no notion of spend or placement
// never carries authority fields it must set to null, and the hub cannot read
// authority out of an envelope whose kind has none.
type WeftJobPayload struct {
	Command           string            `json:"command"`
	WorkingDir        string            `json:"working_dir,omitempty"`
	Project           string            `json:"project,omitempty"`
	Description       string            `json:"description,omitempty"`
	SourceDigest      string            `json:"source_digest,omitempty"`
	QueueParams       json.RawMessage   `json:"queue_params,omitempty"`
	SpendCeilingUSD   float64           `json:"spend_ceiling_usd"`
	TargetConstraints TargetConstraints `json:"target_constraints"`
}

// TargetConstraints is what the edge asks for. It is a request, not a grant:
// the hub checks it against its own allowlist, which no submission can extend.
type TargetConstraints struct {
	// Hosts names acceptable execution targets. Empty means the hub chooses
	// from its allowlist by its normal placement rules.
	Hosts []string `json:"hosts,omitempty"`
	// GPU is a weft GPU constraint expression, e.g. "nvidia>=24GB".
	GPU string `json:"gpu,omitempty"`
	// Tags are weft placement tags carried with the job.
	Tags []string `json:"tags,omitempty"`
}

// ParseWeftJobPayload decodes a verified payload.
//
// It takes the digest-checked bytes only. There is no overload that accepts
// unverified bytes, so a caller cannot parse a payload the hub has not
// confirmed matches the signed pointer.
func ParseWeftJobPayload(data []byte) (*WeftJobPayload, *Refusal) {
	var p WeftJobPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, refuse(ReasonMalformed, "payload is not a valid weft job submission: %v", err)
	}
	if p.Command == "" {
		return nil, refuse(ReasonMalformed, "weft job submission has no command")
	}
	return &p, nil
}

// EncodeWeftJobPayload serializes a job submission for signing.
func EncodeWeftJobPayload(p WeftJobPayload) ([]byte, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encode weft job payload: %w", err)
	}
	return data, nil
}

// PlanEndedPayload asks the hub to close a plan's spend authority.
//
// This is a downward-only request: honoring it can only reduce what the
// submitter may do. That is why it needs no trust beyond the ordinary
// signature, and why it does not weaken the rule that an edge must never
// assert its own liveness.
type PlanEndedPayload struct {
	PlanID string `json:"plan_id"`
	Reason string `json:"reason,omitempty"`
}

// ParsePlanEndedPayload decodes a verified termination request.
func ParsePlanEndedPayload(data []byte) (*PlanEndedPayload, *Refusal) {
	var p PlanEndedPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, refuse(ReasonMalformed, "payload is not a valid plan-ended request: %v", err)
	}
	if p.PlanID == "" {
		return nil, refuse(ReasonMalformed, "plan-ended request names no plan")
	}
	return &p, nil
}

// EncodePlanEndedPayload serializes a termination request for signing.
func EncodePlanEndedPayload(p PlanEndedPayload) ([]byte, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encode plan-ended payload: %w", err)
	}
	return data, nil
}

// ApplyPlanEnded closes the spend authority of the plan that signed the request.
//
// A request may only end its OWN plan. Without that check, anyone who can write
// the inbound bucket could close other plans' authority, turning a safety
// mechanism into a denial-of-service surface. Signed and self-scoped, the worst
// case is that whoever already holds a plan's key can end that plan — which is
// either the plan itself, or a host compromise with better options available.
// Returns a *Refusal for a terminal decision about the submission, and an
// error for a hub-side fault — a missing keyring or a failed write is our
// problem, not a verdict on the submitter's request, and the caller must retry
// rather than record a refusal.
func ApplyPlanEnded(ring *Keyring, v *VerifiedEnvelope, p *PlanEndedPayload, now time.Time) (*Refusal, error) {
	if ring == nil {
		return nil, fmt.Errorf(
			"no keyring is available, so plan %s's authority cannot be closed", v.PlanID())
	}
	if v.PlanID() == "" {
		return refuse(ReasonKeyNotBoundToPlan,
			"key %s is not bound to a plan, so it cannot end one", v.KeyID()), nil
	}
	if p.PlanID != v.PlanID() {
		return refuse(ReasonPlanScopeMismatch,
			"signature valid (key %s, plan %s); a termination request may only end its own plan, not %q",
			v.KeyID(), v.PlanID(), p.PlanID), nil
	}
	// EndAuthority persists, so the closed window survives a hub restart.
	if err := ring.EndAuthority(v.KeyID(), now); err != nil {
		return nil, fmt.Errorf("end authority for key %s: %w", v.KeyID(), err)
	}
	return nil, nil
}
