package edge

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SeenSet records nonces that have already been processed, so that a validly
// signed packet re-uploaded is refused. Signing and replay defense are separate
// mechanisms: a replayed packet's signature is perfectly valid, which is why
// the signature check cannot detect it.
type SeenSet interface {
	// Seen reports whether the nonce has been recorded.
	Seen(nonce string) (bool, error)
	// Record marks the nonce as processed. It must be called before the
	// submission is admitted, so that a crash between the two cannot admit the
	// same submission twice.
	Record(nonce string, submittedAt time.Time) error
}

// VerifyOptions carries what verification needs beyond the object itself.
type VerifyOptions struct {
	Keyring *Keyring
	// Kinds maps payload kinds to their policies. A kind absent from the
	// registry is refused, so admitting a new caller is a deliberate act.
	Kinds *KindRegistry
	// Now is the hub's clock, used only for freshness. Key validity is judged
	// against the envelope's own SubmittedAt.
	Now time.Time
	// DefaultTTL applies to a kind whose policy sets none. Zero disables the
	// freshness check.
	DefaultTTL time.Duration
	// ClockSkew is how far into the future a submission may be dated before it
	// is refused, absorbing ordinary clock drift between edge and hub.
	ClockSkew time.Duration
}

// Verify authenticates a submission object and returns its contents.
//
// The step order is the security property this function exists to provide, and
// every step runs only if all previous steps passed:
//
//  1. frame          — is this even a submission object
//  2. key lookup     — which key, and was it valid when this was signed
//  3. signature      — Ed25519 over the exact envelope octets
//     ---- nothing below this line has influenced anything above it ----
//  4. parse          — only now are the octets interpreted as JSON
//  5. version        — a schema this build implements, or quarantine
//  6. kind           — a payload kind this hub is configured to accept
//  7. host binding   — the claimed submitter matches the key's bound host
//  8. freshness      — within the kind's TTL, not implausibly future-dated
//  9. replay         — nonce not already processed
//
// Steps 1 through 3 are authentication. A VerifiedEnvelope means "we know who
// wrote this" and nothing more; authorization is Authorize, separately.
//
// Returns three outcomes, deliberately distinct. An envelope is confirmed good.
// A *Refusal is a terminal decision about this submission. An error means the
// answer is UNKNOWN — some store could not be consulted — and the caller must
// retry rather than refuse, or a network hiccup would discard valid work.
func Verify(object []byte, seen SeenSet, opts VerifyOptions) (*VerifiedEnvelope, *Refusal, error) {
	// 1. Frame.
	f, refusal := parseFrame(object)
	if refusal != nil {
		return nil, refusal, nil
	}

	// 2. Key lookup. The key id came from outside the signature, so it is a
	// hint: naming the wrong key can only make verification fail.
	key, ok := opts.Keyring.Lookup(f.keyID)
	if !ok {
		return nil, refuse(ReasonUnknownKey,
			"no key with id %q in the keyring; add it with `weft edge key add`", f.keyID), nil
	}
	pub, err := key.parsePublic()
	if err != nil {
		return nil, refuse(ReasonUnknownKey, "keyring entry %s is unusable: %v", f.keyID, err), nil
	}

	// Key validity needs the signing time, which lives inside the envelope and
	// must not be read before the signature verifies. Verify the signature
	// first, then apply the validity window in step 3b.

	// 3. Signature, over the exact octets, still unparsed.
	if !ed25519.Verify(pub, f.envelope, f.signature) {
		return nil, refuse(ReasonBadSignature,
			"signature does not verify under key %s for host %s", key.KeyID, key.Host), nil
	}

	// 4. Parse. Only now are the bytes interpreted.
	var env Envelope
	if err := json.Unmarshal(f.envelope, &env); err != nil {
		return nil, refuse(ReasonMalformed,
			"envelope signed by key %s is not valid JSON: %v", key.KeyID, err), nil
	}

	// 5. Version. Refuse to interpret a schema this build does not implement,
	// and retain the object rather than consuming it.
	if env.ProtocolVersion != ProtocolVersion {
		return nil, quarantine(ReasonUnknownProtocol,
			"envelope declares protocol version %d, this hub implements %d; object retained for a future build",
			env.ProtocolVersion, ProtocolVersion), nil
	}
	if env.Nonce == "" {
		return nil, refuse(ReasonMalformed, "envelope signed by key %s has no nonce", key.KeyID), nil
	}
	if env.SubmittedAt.IsZero() {
		return nil, refuse(ReasonMalformed, "envelope %s has no submitted_at", env.Nonce), nil
	}

	// 6. Kind. Refuse a payload schema this hub is not configured to accept,
	// rather than admitting it under a default.
	kindPolicy, known := opts.Kinds.Lookup(env.PayloadKind)
	if !known {
		return nil, quarantine(ReasonUnknownKind,
			"envelope %s declares payload kind %q, which this hub does not accept; known kinds: %v",
			env.Nonce, env.PayloadKind, opts.Kinds.Known()), nil
	}

	// 7. Host binding. The keyring binds a key to a host; the envelope also
	// states one. A valid key signing a different host's name would otherwise
	// make the hub record provenance it did not observe and cannot check,
	// which is worse than no provenance because it reads as verified.
	// An absent submitter host is a mismatch, not an exemption. Treating it as
	// "nothing to check" would make the binding optional at the submitter's
	// discretion, which is the one party it constrains.
	if !strings.EqualFold(env.SubmitterHost, key.Host) {
		claimed := env.SubmitterHost
		if claimed == "" {
			claimed = "(absent)"
		}
		return nil, refuse(ReasonHostMismatch,
			"envelope %s claims submitter host %s but key %s is bound to host %q",
			env.Nonce, claimed, key.KeyID, key.Host), nil
	}

	// 3b. Key validity window, judged against the signing time now that it is
	// authenticated.
	if refusal := key.usableAt(env.SubmittedAt); refusal != nil {
		return nil, refusal, nil
	}

	// 8. Freshness, against the kind's own time-to-live. Staleness means
	// different things per kind, so the value is configuration rather than a
	// constant.
	ttl := kindPolicy.TTL
	if ttl == 0 {
		ttl = opts.DefaultTTL
	}
	if kindPolicy.NeverExpires {
		ttl = 0
	}
	if ttl > 0 {
		age := opts.Now.Sub(env.SubmittedAt)
		if age > ttl {
			return nil, refuse(ReasonExpired,
				"submission %s of kind %s was signed %s ago, beyond that kind's %s time-to-live",
				env.Nonce, env.PayloadKind, age.Round(time.Second), ttl), nil
		}
	}
	if skew := env.SubmittedAt.Sub(opts.Now); skew > opts.ClockSkew {
		return nil, refuse(ReasonFutureDated,
			"submission %s is dated %s in the future, beyond the %s allowance",
			env.Nonce, skew.Round(time.Second), opts.ClockSkew), nil
	}

	// 9. Replay. A re-uploaded packet carries a valid signature, so only the
	// seen-set can catch it.
	if seen != nil {
		already, err := seen.Seen(env.Nonce)
		if err != nil {
			// An unreadable seen-set is UNKNOWN, not a confirmed replay.
			// Reporting it as a refusal would make a transient read failure
			// permanently discard valid work; the caller must retry instead.
			return nil, nil, fmt.Errorf(
				"cannot determine whether nonce %s was already processed, so this "+
					"submission's status is unknown: %w", env.Nonce, err)
		}
		if already {
			return nil, refuse(ReasonReplayed,
				"nonce %s was already processed; a resubmission with the same nonce is a no-op by design",
				env.Nonce), nil
		}
	}

	return &VerifiedEnvelope{
		envelope:   env,
		keyID:      key.KeyID,
		host:       key.Host,
		kind:       kindPolicy,
		keyCeiling: key.SpendCeilingUSD,
		planID:     key.PlanID,
	}, nil, nil
}
