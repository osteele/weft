package edge

import "time"

// ProtocolVersion is the envelope schema version this build implements.
// A change of signature algorithm is a version bump, not an algorithm field.
const ProtocolVersion = 1

// Envelope is the signed submission descriptor, shared by every caller of this
// protocol. It carries only delivery and provenance: who wrote this, when, what
// schema the payload uses, and which payload it points at.
//
// It deliberately carries no authority fields. Anything that grants or bounds
// permission — an execution target, a spend ceiling — lives in the payload,
// which the signature covers transitively through PayloadDigest and is
// therefore equally non-forgeable. Keeping them out of the shared envelope
// means a caller with no notion of spend or placement never has to null out
// fields that confer authority, which is where a confused-deputy bug lives.
//
// An Envelope value obtained anywhere other than from a VerifiedEnvelope is
// unauthenticated. Construct one directly only when signing.
type Envelope struct {
	ProtocolVersion        int         `json:"protocol_version"`
	PayloadKind            PayloadKind `json:"payload_kind"`
	SubmitterHost          string      `json:"submitter_host"`
	DeploymentSourceDigest string      `json:"deployment_source_digest"`
	Nonce                  string      `json:"nonce"`
	SubmittedAt            time.Time   `json:"submitted_at"`
	PayloadDigest          string      `json:"payload_digest"`
}

// VerifiedEnvelope is an Envelope whose signature has been checked against a
// key that was valid when the envelope was signed.
//
// It cannot be constructed outside this package, so a caller holding one has
// proof that verification ran. This is the type-level half of verify-then-parse;
// the ordering half lives in Verify.
//
// Holding one proves who wrote the submission. It proves nothing about whether
// the work may run: that is Authorize, and a valid signature never skips it.
type VerifiedEnvelope struct {
	envelope Envelope
	keyID    string
	host     string
	kind     KindPolicy
	// keyCeiling is the spend authority granted to this key's plan. It is
	// hub-side configuration the edge never transmits, so it is unforgeable by
	// construction rather than by cryptography.
	keyCeiling float64
	planID     string
}

// PlanID returns the plan execution this submission's key was minted for.
func (v *VerifiedEnvelope) PlanID() string { return v.planID }

// KindPolicy returns the policy for this submission's payload kind.
func (v *VerifiedEnvelope) KindPolicy() KindPolicy { return v.kind }

// Envelope returns the verified contents.
func (v *VerifiedEnvelope) Envelope() Envelope { return v.envelope }

// KeyID returns the keyring entry that verified this submission.
func (v *VerifiedEnvelope) KeyID() string { return v.keyID }

// SigningHost returns the host bound to the verifying key. This is the
// authenticated identity, and it is trustworthy in a way that the envelope's
// own SubmitterHost field is not: the latter is merely signed content, which a
// compromised edge controls.
func (v *VerifiedEnvelope) SigningHost() string { return v.host }
