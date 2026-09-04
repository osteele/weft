package edge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// SubmitRequest is what an edge asks the hub to run.
type SubmitRequest struct {
	SubmitterHost          string
	DeploymentSourceDigest string
	// Kind names the payload schema. The hub routes to a validator by this
	// value rather than inferring one.
	Kind PayloadKind
	// Payload is the already-serialized payload. Authority fields, where the
	// kind has any, live inside it and are covered by the signature through
	// the envelope's payload digest.
	Payload []byte
}

// SubmitResult reports what happened. Committed is false when the pointer
// already existed, which is a successful no-op rather than an error.
type SubmitResult struct {
	Nonce         string
	PayloadDigest string
	Committed     bool
}

// Submit publishes a submission: payload first, pointer last.
//
// The order is load-bearing. An interrupted submission must never leave a
// pointer to a payload that is not fully present, so the payload is written to
// its content-addressed key first, where a partial or repeated write is
// harmless, and the pointer write is the single atomic commit point.
func Submit(ctx context.Context, t Transport, signer *Signer, req SubmitRequest, now time.Time) (*SubmitResult, error) {
	if signer == nil {
		return nil, fmt.Errorf(
			"edge submission requires a signing key; mint one with `weft edge key mint --plan <plan-id>`")
	}
	if req.SubmitterHost == "" {
		return nil, fmt.Errorf("edge submission requires a submitter host")
	}
	if len(req.Payload) == 0 {
		return nil, fmt.Errorf("edge submission requires a payload")
	}
	if req.Kind == "" {
		return nil, fmt.Errorf("edge submission requires a payload kind")
	}

	sum := sha256.Sum256(req.Payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	nonce, err := NewNonce(now)
	if err != nil {
		return nil, err
	}

	// 1. Payload, content-addressed. Rewriting the same bytes to the same key
	// is a no-op, so this step needs no conditional write and is safe to retry.
	if err := t.Put(ctx, PayloadKey(digest), req.Payload); err != nil {
		return nil, fmt.Errorf("upload payload: %w", err)
	}

	env := Envelope{
		ProtocolVersion:        ProtocolVersion,
		PayloadKind:            req.Kind,
		SubmitterHost:          req.SubmitterHost,
		DeploymentSourceDigest: req.DeploymentSourceDigest,
		Nonce:                  nonce,
		SubmittedAt:            now.UTC(),
		PayloadDigest:          digest,
	}
	object, err := signer.Sign(env)
	if err != nil {
		return nil, err
	}

	// 2. Pointer. The commit point.
	err = t.PutIfAbsent(ctx, InboxKey(nonce), object)
	if errors.Is(err, ErrAlreadyExists) {
		// The nonce is freshly generated, so this is all but impossible; if it
		// happens, the submission is already committed and creating a second
		// one would be the bug.
		return &SubmitResult{Nonce: nonce, PayloadDigest: digest, Committed: false}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("commit submission pointer: %w", err)
	}
	return &SubmitResult{Nonce: nonce, PayloadDigest: digest, Committed: true}, nil
}

// FetchPayload retrieves and verifies the payload a verified envelope points
// at.
//
// Three outcomes, deliberately distinct. A payload plus nil refusal is
// confirmed good. A refusal is a confirmed, terminal problem with the
// submission. An error is *unknown* — the store could not be consulted — and
// the caller must retry rather than treat the submission as bad, because a
// network failure and a missing payload are not the same fact.
//
// The digest check is an authorization step, not an integrity nicety: the
// pointer and payload are separate objects, and a writer who could replace the
// payload under a committed pointer would otherwise change what runs.
func FetchPayload(ctx context.Context, t Transport, v *VerifiedEnvelope) ([]byte, *Refusal, error) {
	env := v.Envelope()
	data, err := t.Get(ctx, PayloadKey(env.PayloadDigest))
	if errors.Is(err, ErrNotFound) {
		return nil, refuse(ReasonPayloadMismatch,
			"submission %s points at payload %s, which is not present in the inbound store",
			env.Nonce, env.PayloadDigest), nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("payload %s for submission %s could not be read; "+
			"the store may be unreachable, so this submission's status is unknown: %w",
			env.PayloadDigest, env.Nonce, err)
	}
	sum := sha256.Sum256(data)
	actual := "sha256:" + hex.EncodeToString(sum[:])
	if actual != env.PayloadDigest {
		return nil, refuse(ReasonPayloadMismatch,
			"submission %s declares payload %s but the stored object hashes to %s",
			env.Nonce, env.PayloadDigest, actual), nil
	}
	return data, nil, nil
}
