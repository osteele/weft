package edge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Admission is a submission the hub has decided to run.
type Admission struct {
	Envelope      Envelope
	Authorization Authorization
	Payload       []byte
	SourceClosure []byte
	// Job is the parsed payload for KindWeftJobSubmission.
	Job *WeftJobPayload
	// SigningHost is the authenticated identity, taken from the keyring entry
	// rather than from the envelope's self-reported SubmitterHost. Both are
	// recorded as provenance; only this one is evidence.
	SigningHost string
	KeyID       string
}

// AdmitOptions carries everything hub-side admission needs.
type AdmitOptions struct {
	Verify VerifyOptions
	Policy *Policy
	Seen   SeenSet
}

// Admit runs the full hub-side pipeline for one submission object.
//
// Order: authenticate the envelope, fetch and digest-check the payload, parse
// it under its declared kind, and only then authorize. Authorization comes last
// because the fields it reads live in the payload, and they are trustworthy
// only once the payload has been confirmed to match the signed digest.
//
// A *Refusal is a terminal
// decision about this submission. An error is unknown — the store could not be
// consulted — and the caller must leave the submission for a later pass rather
// than refusing it, because refusing on an unreachable store would discard
// valid work whenever the network hiccuped.
//
// The seen-set is recorded only once every check has passed, so a submission
// refused for a transient reason is not permanently burned, while an admitted
// one is recorded before it can be acted on.
func Admit(ctx context.Context, t Transport, object []byte, opts AdmitOptions) (*Admission, *Refusal, error) {
	if opts.Verify.Keyring == nil {
		return nil, nil, fmt.Errorf("edge admission requires a keyring")
	}
	verified, refusal, err := Verify(object, opts.Seen, opts.Verify)
	if err != nil {
		return nil, nil, err
	}
	if refusal != nil {
		return nil, refusal, nil
	}

	payload, refusal, err := FetchPayload(ctx, t, verified)
	if err != nil {
		return nil, nil, err
	}
	if refusal != nil {
		return nil, refusal, nil
	}

	env := verified.Envelope()

	// Content idempotency, for kinds whose payload is their identity. Two such
	// submissions with different nonces are still one submission, so the nonce
	// seen-set alone would let them both through. The protocol specifies this
	// for every implementation, so a caller registering a content-identity kind
	// on the strength of the spec must actually get it.
	contentKey := ""
	if verified.KindPolicy().ContentIdempotent && opts.Seen != nil {
		contentKey = ContentSeenKey(verified.KeyID(), env.PayloadKind, env.PayloadDigest)
		seen, err := opts.Seen.Seen(contentKey)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"cannot determine whether this %s content was already submitted, so its "+
					"status is unknown: %w", env.PayloadKind, err)
		}
		if seen {
			return nil, refuse(ReasonDuplicateContent,
				"a %s submission with identical content was already processed; this kind "+
					"collapses duplicates by design", env.PayloadKind), nil
		}
	}

	// Dispatch on the declared kind. A kind the registry accepted but that has
	// no handler here is a programming error rather than a bad submission, so
	// it fails loudly rather than being admitted unvalidated.
	var (
		job           *WeftJobPayload
		auth          *Authorization
		sourceClosure []byte
	)
	switch env.PayloadKind {
	case KindWeftJobSubmission:
		job, refusal = ParseWeftJobPayload(payload)
		if refusal != nil {
			return nil, refusal, nil
		}
		auth, refusal = Authorize(verified, job, opts.Policy)
		if refusal != nil {
			return nil, refusal, nil
		}
		if job.SourceDigest != "" {
			sourceClosure, refusal, err = FetchSourceClosure(ctx, t, env.Nonce, job.SourceDigest)
			if err != nil {
				return nil, nil, err
			}
			if refusal != nil {
				return nil, refusal, nil
			}
		}
	case KindPlanEnded:
		ended, refusal := ParsePlanEndedPayload(payload)
		if refusal != nil {
			return nil, refusal, nil
		}
		// Honoring this only ever reduces authority, so it needs no
		// authorization step beyond the signature and the self-scope check.
		refusal, err := ApplyPlanEnded(opts.Verify.Keyring, verified, ended, opts.Verify.Now)
		if err != nil {
			return nil, nil, err
		}
		if refusal != nil {
			return nil, refusal, nil
		}
		auth = &Authorization{}
	default:
		return nil, nil, fmt.Errorf(
			"payload kind %q is registered but has no handler; refusing to admit it unvalidated",
			env.PayloadKind)
	}

	if opts.Seen != nil {
		if err := opts.Seen.Record(env.Nonce, env.SubmittedAt); err != nil {
			// Losing the race to record means another process already admitted
			// this submission. Refuse rather than admit it twice.
			if errors.Is(err, ErrAlreadyRecorded) {
				return nil, refuse(ReasonReplayed,
					"nonce %s was recorded by another hub process while this one was "+
						"admitting it; the submission is already accounted for", env.Nonce), nil
			}
			return nil, nil, err
		}
		if contentKey != "" {
			if err := opts.Seen.Record(contentKey, env.SubmittedAt); err != nil {
				return nil, nil, err
			}
		}
	}

	return &Admission{
		Envelope:      env,
		Authorization: *auth,
		Payload:       payload,
		SourceClosure: sourceClosure,
		Job:           job,
		SigningHost:   verified.SigningHost(),
		KeyID:         verified.KeyID(),
	}, nil, nil
}

// Ack is the hub's acknowledgement, written back to the inbound store so the
// edge can observe progress without ever connecting to the hub.
//
// Acknowledgements are deliberately unsigned. They carry no authority: an edge
// that believed a forged ack would learn a wrong status, not gain a
// capability, and signing them would enlarge the key-handling surface for no
// protection.
type Ack struct {
	Version    int       `json:"version"`
	Nonce      string    `json:"nonce"`
	Accepted   bool      `json:"accepted"`
	ReasonCode string    `json:"reason_code,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	JobID      int64     `json:"job_id,omitempty"`
	Phase      string    `json:"phase,omitempty"`
	HubHost    string    `json:"hub_host,omitempty"`
	AckedAt    time.Time `json:"acked_at"`
	PhaseSince time.Time `json:"phase_since,omitempty"`
}

// ContentSeenKey derives the seen-set key for a content-idempotent submission.
//
// The signing key is part of the key because a kind's effect can be per-signer:
// a plan-ended request closes the authority of the key that signed it, so two
// different keys sending byte-identical content are two different effects and
// must not collapse into one. Scoping by signer keeps collapse to what it is
// for — a retry of the same submission by the same party.
//
// It is hashed rather than concatenated so the result is a single opaque token
// with no separators, which keeps it valid wherever a nonce is valid.
func ContentSeenKey(keyID string, kind PayloadKind, payloadDigest string) string {
	sum := sha256.Sum256([]byte(keyID + "\x00" + string(kind) + "\x00" + payloadDigest))
	return "content-" + hex.EncodeToString(sum[:])
}
