package edge

import "fmt"

// ReasonCode identifies why a submission was refused. Each code maps to one
// distinct operator-facing message; callers must not collapse them into a
// generic rejection, because the remedy differs per code.
type ReasonCode string

const (
	// Authentication failures. Nothing derived from the object's contents has
	// influenced any decision when these are returned.
	ReasonUnframed       ReasonCode = "unframed"
	ReasonUnknownKey     ReasonCode = "unknown_key"
	ReasonKeyNotYetValid ReasonCode = "key_not_yet_valid"
	ReasonKeyExpired     ReasonCode = "key_expired"
	ReasonKeyRevoked     ReasonCode = "key_revoked"
	ReasonBadSignature   ReasonCode = "bad_signature"

	// Schema failures, evaluated only after the signature verified.
	ReasonMalformed       ReasonCode = "malformed"
	ReasonUnknownProtocol ReasonCode = "unknown_protocol_version"
	ReasonUnknownKind     ReasonCode = "unknown_payload_kind"
	// ReasonHostMismatch marks an envelope whose self-reported submitter host
	// disagrees with the host bound to the verifying key.
	ReasonHostMismatch ReasonCode = "submitter_host_mismatch"

	// Freshness and replay.
	ReasonExpired     ReasonCode = "expired"
	ReasonFutureDated ReasonCode = "future_dated"
	ReasonReplayed    ReasonCode = "replayed_nonce"
	// ReasonDuplicateContent marks a resubmission of identical content for a
	// kind whose payload is its identity. Distinct from a replayed nonce:
	// this is a different submission carrying the same fact.
	ReasonDuplicateContent ReasonCode = "duplicate_content"

	// Authorization failures. The writer is authenticated; the work is not
	// permitted. These are never reported as signature problems.
	ReasonTargetNotAllowed ReasonCode = "target_not_allowed"
	// ReasonPlanScopeMismatch marks a request that tried to act on a plan
	// other than the one its key is bound to.
	ReasonPlanScopeMismatch ReasonCode = "plan_scope_mismatch"
	// ReasonKeyNotBoundToPlan marks a key with no plan attempting a
	// plan-scoped operation.
	ReasonKeyNotBoundToPlan ReasonCode = "key_not_bound_to_plan"
	ReasonOverSpendCeiling  ReasonCode = "over_spend_ceiling"
	ReasonPayloadMismatch   ReasonCode = "payload_digest_mismatch"
)

// Refusal is a terminal decision not to admit a submission.
type Refusal struct {
	Code   ReasonCode
	Detail string
	// Quarantine marks a refusal whose object must be retained rather than
	// consumed, because this hub could not interpret it and a future version
	// might.
	Quarantine bool
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("%s: %s", r.Code, r.Detail)
}

func refuse(code ReasonCode, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Detail: fmt.Sprintf(format, args...)}
}

func quarantine(code ReasonCode, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Detail: fmt.Sprintf(format, args...), Quarantine: true}
}

// IsAuthenticationFailure reports whether the refusal concerns who wrote the
// packet, as opposed to whether the work it requests is permitted. Callers use
// this to keep the two apart in operator-facing output.
func (r *Refusal) IsAuthenticationFailure() bool {
	switch r.Code {
	case ReasonUnframed, ReasonUnknownKey, ReasonKeyNotYetValid,
		ReasonKeyExpired, ReasonKeyRevoked, ReasonBadSignature, ReasonHostMismatch:
		return true
	}
	return false
}

// IsAuthorizationFailure reports whether the writer was authenticated but the
// requested work was not permitted.
func (r *Refusal) IsAuthorizationFailure() bool {
	switch r.Code {
	case ReasonTargetNotAllowed, ReasonOverSpendCeiling, ReasonPayloadMismatch,
		ReasonPlanScopeMismatch, ReasonKeyNotBoundToPlan:
		return true
	}
	return false
}
