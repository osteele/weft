package edge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testNow() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }

// harness builds a complete edge/hub pair over the filesystem transport.
// Signature verification is fully enabled here with real test keys; nothing is
// stubbed out, so every refusal path below exercises the production code.
type harness struct {
	t       *testing.T
	tr      *FSTransport
	signer  *Signer
	ring    *Keyring
	policy  *Policy
	seen    *FileSeenSet
	nowFunc func() time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	tr, err := NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	signer, pub, err := GenerateSigner("studio-test", "studio")
	if err != nil {
		t.Fatalf("generate signer: %v", err)
	}
	pub.NotBefore = testNow().Add(-24 * time.Hour)
	pub.NotAfter = testNow().Add(24 * time.Hour)
	pub.PlanID = "plan-default"
	pub.SpendCeilingUSD = 10.0
	ring := NewKeyring()
	if err := ring.Add(pub); err != nil {
		t.Fatalf("add key: %v", err)
	}
	policy, err := NewPolicy("laptop", []string{"studio", "cool30"}, 10.0)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	seen, err := NewFileSeenSet(t.TempDir())
	if err != nil {
		t.Fatalf("seen set: %v", err)
	}
	return &harness{t: t, tr: tr, signer: signer, ring: ring, policy: policy, seen: seen, nowFunc: testNow}
}

func (h *harness) admitOpts() AdmitOptions {
	return AdmitOptions{
		Verify: VerifyOptions{
			Keyring:    h.ring,
			Kinds:      DefaultKindRegistry(),
			Now:        h.nowFunc(),
			DefaultTTL: time.Hour,
			ClockSkew:  time.Minute,
		},
		Policy: h.policy,
		Seen:   h.seen,
	}
}

func (h *harness) submit(req SubmitRequest) *SubmitResult {
	h.t.Helper()
	res, err := Submit(context.Background(), h.tr, h.signer, req, h.nowFunc())
	if err != nil {
		h.t.Fatalf("submit: %v", err)
	}
	return res
}

func defaultRequest() SubmitRequest {
	return jobRequest(WeftJobPayload{
		Command:           "echo hello",
		SpendCeilingUSD:   5.0,
		TargetConstraints: TargetConstraints{Hosts: []string{"studio"}},
	})
}

func jobRequest(job WeftJobPayload) SubmitRequest {
	data, err := EncodeWeftJobPayload(job)
	if err != nil {
		panic(err)
	}
	return SubmitRequest{
		SubmitterHost:          "studio",
		DeploymentSourceDigest: "sha256:deadbeef",
		Kind:                   KindWeftJobSubmission,
		Payload:                data,
	}
}

// admitNonce reads the committed pointer back and runs hub-side admission,
// which is what the hub's poller does.
func (h *harness) admitNonce(nonce string) (*Admission, *Refusal) {
	h.t.Helper()
	object, err := h.tr.Get(context.Background(), InboxKey(nonce))
	if err != nil {
		h.t.Fatalf("read pointer: %v", err)
	}
	adm, refusal, err := Admit(context.Background(), h.tr, object, h.admitOpts())
	if err != nil {
		h.t.Fatalf("admit returned unknown-state error: %v", err)
	}
	return adm, refusal
}

func TestRoundTripAdmitsSignedSubmission(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	if !res.Committed {
		t.Fatal("expected first submission to commit")
	}

	adm, refusal := h.admitNonce(res.Nonce)
	if refusal != nil {
		t.Fatalf("expected admission, got refusal %s: %s", refusal.Code, refusal.Detail)
	}
	if adm.SigningHost != "studio" {
		t.Errorf("signing host = %q, want studio", adm.SigningHost)
	}
	if adm.KeyID != "studio-test" {
		t.Errorf("key id = %q, want studio-test", adm.KeyID)
	}
	if adm.Envelope.DeploymentSourceDigest != "sha256:deadbeef" {
		t.Errorf("deployment digest not recorded: %q", adm.Envelope.DeploymentSourceDigest)
	}
	if adm.Job.Command != "echo hello" {
		t.Errorf("command mismatch: %q", adm.Job.Command)
	}
	if adm.Authorization.EffectiveSpendCeilingUSD != 5.0 {
		t.Errorf("effective ceiling = %v, want 5", adm.Authorization.EffectiveSpendCeilingUSD)
	}
}

func TestAdmitFetchesAndDigestChecksSourceClosure(t *testing.T) {
	h := newHarness(t)
	closure := []byte("source closure")
	sum := sha256.Sum256(closure)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	request := jobRequest(WeftJobPayload{
		Command: "echo hello", SourceDigest: digest, SpendCeilingUSD: 5,
		TargetConstraints: TargetConstraints{Hosts: []string{"studio"}},
	})
	request.SourceClosure = closure
	request.SourceDigest = digest
	res := h.submit(request)
	if err := h.tr.Put(context.Background(), SourceKey(digest), []byte("tampered source closure")); err != nil {
		t.Fatalf("tamper source object: %v", err)
	}

	_, refusal := h.admitNonce(res.Nonce)
	if refusal == nil || refusal.Code != ReasonPayloadMismatch {
		t.Fatalf("refusal = %#v, want %s", refusal, ReasonPayloadMismatch)
	}
	seen, err := h.seen.Seen(res.Nonce)
	if err != nil {
		t.Fatalf("Seen: %v", err)
	}
	if seen {
		t.Fatal("source mismatch was recorded as admitted")
	}
}

// A retried submission with the same nonce must create no second job. This is
// the property that stops a retry loop from launching three paid rentals.
func TestReplayedNonceRefusedAfterAdmission(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())

	if _, refusal := h.admitNonce(res.Nonce); refusal != nil {
		t.Fatalf("first admission refused: %s", refusal.Code)
	}
	adm, refusal := h.admitNonce(res.Nonce)
	if adm != nil {
		t.Fatal("second admission of the same nonce produced a second job")
	}
	if refusal == nil || refusal.Code != ReasonReplayed {
		t.Fatalf("want ReasonReplayed, got %v", refusal)
	}
}

// Re-committing the same pointer key is a no-op, not an error: the store, not
// just the hub, enforces idempotency.
func TestPutIfAbsentRefusesSecondPointerWrite(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	err := h.tr.PutIfAbsent(context.Background(), InboxKey(res.Nonce), []byte("second"))
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("want ErrAlreadyExists, got %v", err)
	}
}

func TestUnsignedObjectRefused(t *testing.T) {
	h := newHarness(t)
	_, refusal, err := Admit(context.Background(), h.tr, []byte(`{"protocol_version":1}`), h.admitOpts())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if refusal == nil || refusal.Code != ReasonUnframed {
		t.Fatalf("want ReasonUnframed, got %v", refusal)
	}
	if !refusal.IsAuthenticationFailure() {
		t.Error("unframed object should classify as an authentication failure")
	}
}

func TestBadSignatureRefused(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))

	// Flip a byte inside the envelope; the signature no longer covers it.
	tampered := append([]byte(nil), object...)
	tampered[len(tampered)-3] ^= 0xFF

	_, refusal, err := Admit(context.Background(), h.tr, tampered, h.admitOpts())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if refusal == nil || refusal.Code != ReasonBadSignature {
		t.Fatalf("want ReasonBadSignature, got %v", refusal)
	}
}

// The ordering property, tested directly. This envelope declares a protocol
// version the hub does not implement AND carries a broken signature. If
// verification ran first, as it must, the answer is bad signature. If anything
// parsed the bytes first, the answer would be unknown protocol version — which
// would mean an unauthenticated attacker's field had steered a decision.
func TestSignatureCheckedBeforeAnythingIsParsed(t *testing.T) {
	h := newHarness(t)
	env := Envelope{
		ProtocolVersion: 999,
		PayloadKind:     KindWeftJobSubmission,
		SubmitterHost:   "studio",
		Nonce:           "01JBQTESTNONCE0000000000000",
		SubmittedAt:     testNow(),
		PayloadDigest:   "sha256:whatever",
	}
	octets, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	object := encodeFrame("studio-test", make([]byte, 64), octets)

	_, refusal, err := Admit(context.Background(), h.tr, object, h.admitOpts())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if refusal == nil {
		t.Fatal("expected refusal")
	}
	if refusal.Code == ReasonUnknownProtocol {
		t.Fatal("protocol version was evaluated before the signature verified: " +
			"unverified bytes influenced a decision")
	}
	if refusal.Code != ReasonBadSignature {
		t.Fatalf("want ReasonBadSignature, got %s", refusal.Code)
	}
}

// A correctly signed envelope with an unknown version is quarantined, not
// deleted, so a later build can still process it.
func TestUnknownProtocolVersionQuarantined(t *testing.T) {
	h := newHarness(t)
	env := Envelope{
		ProtocolVersion: 999,
		PayloadKind:     KindWeftJobSubmission,
		SubmitterHost:   "studio",
		Nonce:           "01JBQTESTNONCE0000000000000",
		SubmittedAt:     testNow(),
		PayloadDigest:   "sha256:whatever",
	}
	object, err := h.signer.Sign(env)
	if err != nil {
		t.Fatal(err)
	}
	_, refusal, err := Admit(context.Background(), h.tr, object, h.admitOpts())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if refusal == nil || refusal.Code != ReasonUnknownProtocol {
		t.Fatalf("want ReasonUnknownProtocol, got %v", refusal)
	}
	if !refusal.Quarantine {
		t.Error("unknown protocol version must quarantine, not consume, the object")
	}
}

func TestExpiredSubmissionRefused(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))

	opts := h.admitOpts()
	opts.Verify.Now = testNow().Add(2 * time.Hour) // TTL is one hour

	_, refusal, err := Admit(context.Background(), h.tr, object, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if refusal == nil || refusal.Code != ReasonExpired {
		t.Fatalf("want ReasonExpired, got %v", refusal)
	}
}

func TestDisallowedTargetRefusedAsAuthorization(t *testing.T) {
	h := newHarness(t)
	res := h.submit(jobRequest(WeftJobPayload{
		Command:           "echo hello",
		SpendCeilingUSD:   5.0,
		TargetConstraints: TargetConstraints{Hosts: []string{"cool100"}},
	}))

	_, refusal := h.admitNonce(res.Nonce)
	if refusal == nil || refusal.Code != ReasonTargetNotAllowed {
		t.Fatalf("want ReasonTargetNotAllowed, got %v", refusal)
	}
	if refusal.IsAuthenticationFailure() {
		t.Error("a disallowed target is an authorization failure, not an authentication one")
	}
	if !strings.Contains(refusal.Detail, "signature valid") {
		t.Errorf("refusal must state that the signature was valid, so an operator "+
			"does not chase a key problem: %q", refusal.Detail)
	}
}

// The hub can never be an execution target, even if configuration tries.
func TestPolicyRefusesHubAsTarget(t *testing.T) {
	if _, err := NewPolicy("laptop", []string{"studio", "laptop"}, 10.0); err == nil {
		t.Fatal("policy accepted the hub as an allowed execution target")
	}
}

func TestOverSpendCeilingRefused(t *testing.T) {
	h := newHarness(t)
	res := h.submit(jobRequest(WeftJobPayload{
		Command:           "echo hello",
		SpendCeilingUSD:   500.0, // policy maximum is 10
		TargetConstraints: TargetConstraints{Hosts: []string{"studio"}},
	}))

	_, refusal := h.admitNonce(res.Nonce)
	if refusal == nil || refusal.Code != ReasonOverSpendCeiling {
		t.Fatalf("want ReasonOverSpendCeiling, got %v", refusal)
	}
	if !strings.Contains(refusal.Detail, "signature valid") {
		t.Errorf("refusal must separate signing from authorization: %q", refusal.Detail)
	}
}

// A signed ceiling above the hub maximum is refused rather than clamped, so a
// submission never runs under a budget its author did not choose.
func TestSignedCeilingIsNotSilentlyClamped(t *testing.T) {
	h := newHarness(t)
	res := h.submit(jobRequest(WeftJobPayload{
		Command:         "echo hello",
		SpendCeilingUSD: 11.0, // policy maximum is 10
	}))
	adm, refusal := h.admitNonce(res.Nonce)
	if adm != nil {
		t.Fatal("ceiling above the hub maximum was accepted")
	}
	if refusal == nil || refusal.Code != ReasonOverSpendCeiling {
		t.Fatalf("want ReasonOverSpendCeiling, got %v", refusal)
	}
}

func TestUnknownKeyRefused(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))

	opts := h.admitOpts()
	opts.Verify.Keyring = NewKeyring()

	_, refusal, err := Admit(context.Background(), h.tr, object, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if refusal == nil || refusal.Code != ReasonUnknownKey {
		t.Fatalf("want ReasonUnknownKey, got %v", refusal)
	}
}

// Key validity is judged against the signing time, so a submission made while
// a key was valid still verifies after the key expires.
func TestKeyValidityUsesSigningTimeNotProcessingTime(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))

	key, _ := h.ring.Lookup("studio-test")
	key.NotAfter = testNow().Add(time.Minute)
	if err := h.ring.Add(key); err != nil {
		t.Fatal(err)
	}

	opts := h.admitOpts()
	opts.Verify.Now = testNow().Add(30 * time.Minute)
	opts.Verify.DefaultTTL = 24 * time.Hour // isolate the key check from freshness

	if _, refusal, _ := Admit(context.Background(), h.tr, object, opts); refusal != nil {
		t.Fatalf("submission signed while the key was valid was refused: %s", refusal.Code)
	}
}

// Revocation is the deliberate exception: it refuses even signatures that
// predate it, because a revoked key's past signatures are no longer evidence.
func TestRevocationRefusesEarlierSignatures(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))

	if err := h.ring.Revoke("studio-test", testNow().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, refusal, err := Admit(context.Background(), h.tr, object, h.admitOpts())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if refusal == nil || refusal.Code != ReasonKeyRevoked {
		t.Fatalf("want ReasonKeyRevoked, got %v", refusal)
	}
}

func TestPayloadDigestMismatchRefused(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())

	// Replace the payload under the committed pointer.
	if err := h.tr.Put(context.Background(), PayloadKey(res.PayloadDigest), []byte("different")); err != nil {
		t.Fatal(err)
	}
	_, refusal := h.admitNonce(res.Nonce)
	if refusal == nil || refusal.Code != ReasonPayloadMismatch {
		t.Fatalf("want ReasonPayloadMismatch, got %v", refusal)
	}
}

// A missing payload is a refusal; an unreachable store is an error. Collapsing
// them would let a network failure discard valid work.
func TestMissingPayloadRefusesButDoesNotError(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	if err := h.tr.Delete(context.Background(), PayloadKey(res.PayloadDigest)); err != nil {
		t.Fatal(err)
	}
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))
	_, refusal, err := Admit(context.Background(), h.tr, object, h.admitOpts())
	if err != nil {
		t.Fatalf("confirmed-absent payload must refuse, not error: %v", err)
	}
	if refusal == nil || refusal.Code != ReasonPayloadMismatch {
		t.Fatalf("want ReasonPayloadMismatch, got %v", refusal)
	}
}

func TestPayloadIsContentAddressed(t *testing.T) {
	h := newHarness(t)
	req := defaultRequest()
	res := h.submit(req)
	sum := sha256.Sum256(req.Payload)
	want := "sha256:" + hex.EncodeToString(sum[:])
	if res.PayloadDigest != want {
		t.Errorf("digest = %s, want %s", res.PayloadDigest, want)
	}
}

// Every refusal code must produce a distinct, non-empty message. An operator
// who cannot tell a key problem from a budget problem cannot act.
func TestRefusalCodesAreDistinct(t *testing.T) {
	seen := map[ReasonCode]bool{}
	codes := []ReasonCode{
		ReasonUnframed, ReasonUnknownKey, ReasonKeyNotYetValid, ReasonKeyExpired,
		ReasonKeyRevoked, ReasonBadSignature, ReasonMalformed, ReasonUnknownProtocol,
		ReasonExpired, ReasonFutureDated, ReasonReplayed, ReasonTargetNotAllowed,
		ReasonOverSpendCeiling, ReasonPayloadMismatch, ReasonUnknownKind,
		ReasonHostMismatch,
	}
	for _, code := range codes {
		if code == "" {
			t.Error("empty reason code")
		}
		if seen[code] {
			t.Errorf("duplicate reason code %q", code)
		}
		seen[code] = true
	}
	for _, code := range codes {
		r := &Refusal{Code: code, Detail: "x"}
		if r.IsAuthenticationFailure() && r.IsAuthorizationFailure() {
			t.Errorf("%s classifies as both authentication and authorization", code)
		}
	}
}

func TestSeenSetRetentionMustExceedTTL(t *testing.T) {
	h := newHarness(t)
	if _, err := h.seen.Prune(time.Minute, time.Hour, testNow()); err == nil {
		t.Fatal("prune accepted a retention window shorter than the TTL, " +
			"which would make still-fresh submissions replayable")
	}
}

func TestNoncesAreUniqueAndSortable(t *testing.T) {
	seen := map[string]bool{}
	prev := ""
	for i := range 100 {
		n, err := NewNonce(testNow().Add(time.Duration(i) * time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if len(n) != 26 {
			t.Fatalf("nonce %q is %d chars, want 26", n, len(n))
		}
		if seen[n] {
			t.Fatalf("duplicate nonce %q", n)
		}
		seen[n] = true
		if prev != "" && n <= prev {
			t.Fatalf("nonce %q does not sort after %q", n, prev)
		}
		prev = n
	}
}

// A valid key must not be able to sign another host's name into the provenance
// record. Provenance is a fact the hub asserts about work it did not observe,
// so an unchecked claim there is worse than none: it reads as verified.
func TestSubmitterHostMustMatchTheKeysBoundHost(t *testing.T) {
	h := newHarness(t)
	req := defaultRequest()
	req.SubmitterHost = "cool30" // the key is bound to studio
	res := h.submit(req)

	_, refusal := h.admitNonce(res.Nonce)
	if refusal == nil || refusal.Code != ReasonHostMismatch {
		t.Fatalf("want ReasonHostMismatch, got %v", refusal)
	}
	if !refusal.IsAuthenticationFailure() {
		t.Error("a forged submitter host is an authentication failure")
	}
}

// An unregistered payload kind is quarantined, not admitted under a default.
func TestUnknownPayloadKindQuarantined(t *testing.T) {
	h := newHarness(t)
	req := defaultRequest()
	req.Kind = "agent-review.ledger-row/v1" // not registered on this hub
	res := h.submit(req)

	_, refusal := h.admitNonce(res.Nonce)
	if refusal == nil || refusal.Code != ReasonUnknownKind {
		t.Fatalf("want ReasonUnknownKind, got %v", refusal)
	}
	if !refusal.Quarantine {
		t.Error("an unknown kind must quarantine rather than consume the object")
	}
}

// Per-kind TTL: the same mechanism carries different values, because staleness
// means different things to different callers.
func TestPerKindTTLOverridesTheDefault(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))

	opts := h.admitOpts()
	opts.Verify.Kinds = NewKindRegistry()
	opts.Verify.Kinds.Register(KindWeftJobSubmission, KindPolicy{TTL: 24 * time.Hour})
	opts.Verify.DefaultTTL = time.Minute
	opts.Verify.Now = testNow().Add(2 * time.Hour)

	if _, refusal, _ := Admit(context.Background(), h.tr, object, opts); refusal != nil {
		t.Fatalf("kind TTL of 24h should have admitted a 2h-old submission, got %s", refusal.Code)
	}
}

// Authority fields moved into the payload keep their non-forgeability, because
// the signed envelope names the payload by digest.
func TestTamperingWithPayloadAuthorityFieldsIsDetected(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())

	raised, err := EncodeWeftJobPayload(WeftJobPayload{
		Command:           "echo hello",
		SpendCeilingUSD:   9999.0,
		TargetConstraints: TargetConstraints{Hosts: []string{"studio"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite the payload the committed pointer names, without re-signing.
	if err := h.tr.Put(context.Background(), PayloadKey(res.PayloadDigest), raised); err != nil {
		t.Fatal(err)
	}
	_, refusal := h.admitNonce(res.Nonce)
	if refusal == nil || refusal.Code != ReasonPayloadMismatch {
		t.Fatalf("raising a ceiling by rewriting the payload must be detected, got %v", refusal)
	}
}

// Ending a plan normally must not repudiate what it already submitted. This
// works only because validity is judged against the envelope's submitted_at
// rather than the hub's processing clock; if that ever changes, this test is
// the one that notices.
func TestEndingAuthorityDoesNotRepudiatePastSubmissions(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))

	// The plan ends one minute after this submission was signed.
	if err := h.ring.EndAuthority("studio-test", testNow().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	opts := h.admitOpts()
	opts.Verify.Now = testNow().Add(30 * time.Minute)

	if _, refusal, _ := Admit(context.Background(), h.tr, object, opts); refusal != nil {
		t.Fatalf("work submitted while the plan held authority was repudiated by its ending: %s",
			refusal.Code)
	}
}

// A session that keeps running after its plan ended cannot submit.
func TestSubmissionAfterAuthorityEndsIsRefused(t *testing.T) {
	h := newHarness(t)
	if err := h.ring.EndAuthority("studio-test", testNow().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	res := h.submit(defaultRequest())

	_, refusal := h.admitNonce(res.Nonce)
	if refusal == nil || refusal.Code != ReasonKeyExpired {
		t.Fatalf("want ReasonKeyExpired, got %v", refusal)
	}
	// The message must tell an autonomous submitter that its authority ended,
	// not that its request was bad, or it will retry forever.
	if !strings.Contains(refusal.Detail, "not a problem with the submission") {
		t.Errorf("lapse refusal must distinguish itself from a malformed request: %q", refusal.Detail)
	}
}

// Revocation still repudiates, and remains distinct from a lapsed window.
func TestRevocationAndLapseAreDistinct(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))

	if err := h.ring.EndAuthority("studio-test", testNow().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, refusal, _ := Admit(context.Background(), h.tr, object, h.admitOpts()); refusal != nil {
		t.Fatalf("lapse should not repudiate past work: %s", refusal.Code)
	}
	if err := h.ring.Revoke("studio-test", testNow().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, refusal, _ := Admit(context.Background(), h.tr, object, h.admitOpts())
	if refusal == nil || refusal.Code != ReasonKeyRevoked {
		t.Fatalf("revocation must repudiate the same submission a lapse allowed, got %v", refusal)
	}
}

func TestRenewSlidesTheWindowForward(t *testing.T) {
	h := newHarness(t)
	cfg := DefaultLeaseConfig()
	if err := h.ring.Renew("studio-test", testNow(), cfg); err != nil {
		t.Fatal(err)
	}
	key, _ := h.ring.Lookup("studio-test")
	if want := testNow().Add(cfg.Window); !key.NotAfter.Equal(want) {
		t.Errorf("not_after = %s, want %s", key.NotAfter, want)
	}
}

// Renewal must not quietly undo a statement that the key is compromised.
func TestRenewRefusesARevokedKey(t *testing.T) {
	h := newHarness(t)
	if err := h.ring.Revoke("studio-test", testNow()); err != nil {
		t.Fatal(err)
	}
	if err := h.ring.Renew("studio-test", testNow(), DefaultLeaseConfig()); err == nil {
		t.Fatal("renewal resurrected a revoked key")
	}
}

// The window floor is set by clock skew, not by security: below it, ordinary
// host drift produces spurious refusals.
func TestLeaseWindowFloorIsEnforced(t *testing.T) {
	if err := (LeaseConfig{Window: time.Minute}).Validate(); err == nil {
		t.Fatal("accepted a lease window below the clock-skew floor")
	}
	if err := DefaultLeaseConfig().Validate(); err != nil {
		t.Fatalf("default lease config is invalid: %v", err)
	}
}

// The window is a crash backstop, so it must be long enough that a plan running
// normally is not interrupted by it. Nothing renews automatically, so a window
// shorter than a working day would lapse live plans and require manual repair.
func TestDefaultWindowIsABackstopNotAHeartbeat(t *testing.T) {
	if w := DefaultLeaseConfig().Window; w < 12*time.Hour {
		t.Errorf("default window is %s; with no automatic renewal a short window "+
			"lapses plans that are merely running long", w)
	}
}

// Spend authority comes from the key, which the edge never transmits.
func TestSpendCeilingComesFromTheKeyNotTheSubmission(t *testing.T) {
	h := newHarness(t)
	key, _ := h.ring.Lookup("studio-test")
	key.SpendCeilingUSD = 2.0 // this plan was granted less than the hub maximum
	key.PlanID = "plan-42"
	if err := h.ring.Add(key); err != nil {
		t.Fatal(err)
	}
	res := h.submit(jobRequest(WeftJobPayload{
		Command:         "echo hello",
		SpendCeilingUSD: 5.0, // under the hub max of 10, over this plan's grant
	}))
	adm, refusal := h.admitNonce(res.Nonce)
	if adm != nil {
		t.Fatal("a submission exceeding its plan's granted ceiling was admitted")
	}
	if refusal == nil || refusal.Code != ReasonOverSpendCeiling {
		t.Fatalf("want ReasonOverSpendCeiling, got %v", refusal)
	}
	if !strings.Contains(refusal.Detail, "plan-42") {
		t.Errorf("refusal should name the plan whose grant was exceeded: %q", refusal.Detail)
	}
}

// Requesting less than the grant is always safe and is honored exactly.
func TestSubmissionMayRequestLessThanItsGrant(t *testing.T) {
	h := newHarness(t)
	key, _ := h.ring.Lookup("studio-test")
	key.SpendCeilingUSD = 8.0
	if err := h.ring.Add(key); err != nil {
		t.Fatal(err)
	}
	res := h.submit(jobRequest(WeftJobPayload{Command: "echo hello", SpendCeilingUSD: 3.0}))
	adm, refusal := h.admitNonce(res.Nonce)
	if refusal != nil {
		t.Fatalf("unexpected refusal: %s", refusal.Detail)
	}
	if adm.Authorization.EffectiveSpendCeilingUSD != 3.0 {
		t.Errorf("effective ceiling = %v, want the requested 3", adm.Authorization.EffectiveSpendCeilingUSD)
	}
}

// A submission that names no ceiling inherits its plan's grant.
func TestSubmissionWithoutACeilingInheritsThePlanGrant(t *testing.T) {
	h := newHarness(t)
	key, _ := h.ring.Lookup("studio-test")
	key.SpendCeilingUSD = 4.0
	if err := h.ring.Add(key); err != nil {
		t.Fatal(err)
	}
	res := h.submit(jobRequest(WeftJobPayload{Command: "echo hello"}))
	adm, refusal := h.admitNonce(res.Nonce)
	if refusal != nil {
		t.Fatalf("unexpected refusal: %s", refusal.Detail)
	}
	if adm.Authorization.EffectiveSpendCeilingUSD != 4.0 {
		t.Errorf("effective ceiling = %v, want the plan grant of 4", adm.Authorization.EffectiveSpendCeilingUSD)
	}
}

func planEndedRequest(planID string) SubmitRequest {
	data, err := EncodePlanEndedPayload(PlanEndedPayload{PlanID: planID, Reason: "completed"})
	if err != nil {
		panic(err)
	}
	return SubmitRequest{
		SubmitterHost:          "studio",
		DeploymentSourceDigest: "sha256:deadbeef",
		Kind:                   KindPlanEnded,
		Payload:                data,
	}
}

// planKey binds the harness key to a plan with a spend grant.
func (h *harness) planKey(planID string, ceiling float64) {
	h.t.Helper()
	key, _ := h.ring.Lookup("studio-test")
	key.PlanID = planID
	key.SpendCeilingUSD = ceiling
	if err := h.ring.Add(key); err != nil {
		h.t.Fatal(err)
	}
}

// A plan can close its own spend authority. This is the normal path when a plan
// reaches terminal disposition on the edge, and it matters because the grant is
// per plan while the keyring is on the hub.
func TestPlanCanEndItsOwnAuthority(t *testing.T) {
	h := newHarness(t)
	h.planKey("plan-7", 5.0)

	res := h.submit(planEndedRequest("plan-7"))
	if _, refusal := h.admitNonce(res.Nonce); refusal != nil {
		t.Fatalf("plan could not end its own authority: %s", refusal.Detail)
	}

	// A job signed after the plan ended is now refused. The boundary is
	// exclusive: work signed at the closing instant still stands.
	later, err := Submit(context.Background(), h.tr, h.signer, defaultRequest(),
		testNow().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	opts := h.admitOpts()
	opts.Verify.Now = testNow().Add(2 * time.Minute)
	object, _ := h.tr.Get(context.Background(), InboxKey(later.Nonce))
	_, refusal, err := Admit(context.Background(), h.tr, object, opts)
	if err != nil {
		t.Fatal(err)
	}
	if refusal == nil || refusal.Code != ReasonKeyExpired {
		t.Fatalf("work submitted after the plan ended should be refused, got %v", refusal)
	}
}

// The closing instant itself is inclusive: work signed exactly at the end of a
// plan's window is still admitted, because ending authority bounds what comes
// after rather than repudiating what came before.
func TestWorkSignedAtTheClosingInstantStillAdmits(t *testing.T) {
	h := newHarness(t)
	h.planKey("plan-7", 5.0)
	res := h.submit(defaultRequest())
	if err := h.ring.EndAuthority("studio-test", testNow()); err != nil {
		t.Fatal(err)
	}
	if _, refusal := h.admitNonce(res.Nonce); refusal != nil {
		t.Fatalf("work signed at the closing instant was refused: %s", refusal.Code)
	}
}

// Ending a plan must not repudiate what it already submitted.
func TestPlanEndDoesNotRepudiateCompletedWork(t *testing.T) {
	h := newHarness(t)
	h.planKey("plan-7", 5.0)

	job := h.submit(defaultRequest())
	jobObject, _ := h.tr.Get(context.Background(), InboxKey(job.Nonce))

	end := h.submit(planEndedRequest("plan-7"))
	if _, refusal := h.admitNonce(end.Nonce); refusal != nil {
		t.Fatalf("plan end refused: %s", refusal.Detail)
	}

	// The earlier job, signed while the plan held authority, still admits.
	freshSeen, err := NewFileSeenSet(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := h.admitOpts()
	opts.Seen = freshSeen
	if _, refusal, _ := Admit(context.Background(), h.tr, jobObject, opts); refusal != nil {
		t.Fatalf("ending a plan repudiated work it legitimately submitted: %s", refusal.Code)
	}
}

// A termination request may only end its own plan. Otherwise anyone who can
// write the bucket could close other plans' authority.
func TestPlanEndCannotEndAnotherPlan(t *testing.T) {
	h := newHarness(t)
	h.planKey("plan-7", 5.0)

	res := h.submit(planEndedRequest("plan-99"))
	_, refusal := h.admitNonce(res.Nonce)
	if refusal == nil {
		t.Fatal("a key ended a plan it was not bound to")
	}
	if !strings.Contains(refusal.Detail, "only end its own plan") {
		t.Errorf("refusal should name the self-scope rule: %q", refusal.Detail)
	}
}

// The termination path cannot be used to raise authority, only to lower it.
func TestPlanEndCannotExtendAuthority(t *testing.T) {
	h := newHarness(t)
	h.planKey("plan-7", 5.0)
	before, _ := h.ring.Lookup("studio-test")

	res := h.submit(planEndedRequest("plan-7"))
	if _, refusal := h.admitNonce(res.Nonce); refusal != nil {
		t.Fatalf("plan end refused: %s", refusal.Detail)
	}
	after, _ := h.ring.Lookup("studio-test")

	if after.NotAfter.After(before.NotAfter) {
		t.Fatal("a termination request extended the admission window")
	}
	if after.SpendCeilingUSD > before.SpendCeilingUSD {
		t.Fatal("a termination request raised the spend ceiling")
	}
}

// Ending authority on a key whose window has ALREADY lapsed must not widen it.
//
// The previous version of the test above asserted this invariant but could not
// fail: its fixture put the key's window in the future, which is the one case
// where an unconditional assignment happens to narrow. The reachable path is a
// late plan-ended submission, whose kind TTL is four times the lease window.
func TestEndAuthorityNeverWidensALapsedWindow(t *testing.T) {
	h := newHarness(t)
	key, _ := h.ring.Lookup("studio-test")
	lapsed := testNow().Add(-2 * time.Hour)
	key.NotAfter = lapsed
	key.PlanID = "plan-7"
	if err := h.ring.Add(key); err != nil {
		t.Fatal(err)
	}

	// The hub processes a termination request two hours after the lapse.
	if err := h.ring.EndAuthority("studio-test", testNow()); err != nil {
		t.Fatal(err)
	}
	after, _ := h.ring.Lookup("studio-test")
	if after.NotAfter.After(lapsed) {
		t.Fatalf("ending authority widened a lapsed window from %s to %s, "+
			"retroactively admitting everything signed in between",
			lapsed, after.NotAfter)
	}
}

// The same widening, reached through a signed submission rather than the CLI.
func TestLatePlanEndedCannotReadmitTheLapsedInterval(t *testing.T) {
	h := newHarness(t)
	key, _ := h.ring.Lookup("studio-test")
	lapsed := testNow().Add(-time.Hour)
	key.NotAfter = lapsed
	key.PlanID = "plan-7"
	if err := h.ring.Add(key); err != nil {
		t.Fatal(err)
	}
	// Signed while the key was still valid, delivered late.
	res, err := Submit(context.Background(), h.tr, h.signer, planEndedRequest("plan-7"),
		lapsed.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))
	opts := h.admitOpts()
	opts.Verify.Now = testNow()
	if _, _, err := Admit(context.Background(), h.tr, object, opts); err != nil {
		t.Fatal(err)
	}
	after, _ := h.ring.Lookup("studio-test")
	if after.NotAfter.After(lapsed) {
		t.Fatalf("a late plan-ended widened the window from %s to %s", lapsed, after.NotAfter)
	}
}

// Renewing a lapsed key would readmit everything signed during the gap.
func TestRenewRefusesALapsedKey(t *testing.T) {
	h := newHarness(t)
	key, _ := h.ring.Lookup("studio-test")
	key.NotAfter = testNow().Add(-time.Hour)
	key.PlanID = "plan-7"
	if err := h.ring.Add(key); err != nil {
		t.Fatal(err)
	}
	err := h.ring.Renew("studio-test", testNow(), DefaultLeaseConfig())
	if err == nil {
		t.Fatal("renewal resurrected a lapsed key, readmitting the gap")
	}
	if !strings.Contains(err.Error(), "lapsed") {
		t.Errorf("error should name the lapse: %v", err)
	}
}

// --- Regressions for the findings raised in review fa330e2e ---

// An unreadable seen-set is unknown, not a confirmed replay. Reporting it as a
// refusal would permanently discard valid work on a transient read failure.
type brokenSeenSet struct{}

func (brokenSeenSet) Seen(string) (bool, error) {
	return false, errors.New("seen-set unreadable")
}
func (brokenSeenSet) Record(string, time.Time) error { return nil }

func TestUnreadableSeenSetIsUnknownNotRefused(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))

	opts := h.admitOpts()
	opts.Seen = brokenSeenSet{}

	adm, refusal, err := Admit(context.Background(), h.tr, object, opts)
	if adm != nil {
		t.Fatal("admitted despite being unable to check for replay")
	}
	if refusal != nil {
		t.Fatalf("an unreadable seen-set must not be a terminal refusal, got %s: %s",
			refusal.Code, refusal.Detail)
	}
	if err == nil {
		t.Fatal("an unreadable seen-set must surface as an error so the caller retries")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error should say the status is unknown: %v", err)
	}
}

// Re-registering a key that was revoked as compromised must not resurrect it.
func TestRevokedKeyCannotBeReRegistered(t *testing.T) {
	h := newHarness(t)
	if err := h.ring.Revoke("studio-test", testNow()); err != nil {
		t.Fatal(err)
	}
	key, _ := h.ring.Lookup("studio-test")
	fresh := Key{
		KeyID:     key.KeyID,
		Host:      key.Host,
		PublicKey: key.PublicKey,
		NotBefore: testNow(),
		NotAfter:  testNow().Add(6 * time.Hour),
	}
	if err := h.ring.Add(fresh); err == nil {
		t.Fatal("re-registering un-revoked a key that was revoked as compromised")
	}
	after, _ := h.ring.Lookup("studio-test")
	if after.RevokedAt == nil {
		t.Fatal("revocation was cleared")
	}
}

// A malformed key file is skipped and reported, not fatal. Otherwise one bad
// row disables the commands an operator would use to find it.
func TestMalformedKeyFileDoesNotDisableTheKeyring(t *testing.T) {
	dir := t.TempDir()
	_, good, err := GenerateSigner("studio-plan-1", "studio")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveKey(dir, good); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A syntactically valid file whose key is the wrong length.
	if err := os.WriteFile(filepath.Join(dir, "short.json"),
		[]byte(`{"key_id":"x","host":"h","public_key":"YWJj"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ring, err := LoadKeyring(dir)
	if err != nil {
		t.Fatalf("one bad key file took the whole keyring offline: %v", err)
	}
	if len(ring.List()) != 1 {
		t.Errorf("expected the good key to load, got %d keys", len(ring.List()))
	}
	if len(ring.Problems) != 2 {
		t.Errorf("expected 2 reported problems, got %d: %v", len(ring.Problems), ring.Problems)
	}
}

// The key id is derived identically on both sides, so a mint and a registration
// for the same host and plan agree without transmitting it.
func TestKeyIDDerivationAgreesAcrossSides(t *testing.T) {
	if got, want := KeyIDFor("studio", "plan-7"), "studio-plan-7"; got != want {
		t.Fatalf("KeyIDFor = %q, want %q", got, want)
	}
	_, pub, err := GenerateSigner(KeyIDFor("studio", "plan-7"), "studio")
	if err != nil {
		t.Fatal(err)
	}
	if pub.KeyID != KeyIDFor("studio", "plan-7") {
		t.Errorf("mint and registration disagree: %q vs %q", pub.KeyID, KeyIDFor("studio", "plan-7"))
	}
}

// An edge configured for one plan must refuse a key minted for another, rather
// than signing under the wrong plan's identity and spend grant.
func TestRuntimeRefusesAKeyFromAnotherPlan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "signing-key.json")
	signer, _, err := GenerateSigner(KeyIDFor("studio", "plan-1"), "studio")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSigner(path, signer, "studio"); err != nil {
		t.Fatal(err)
	}
	tr, err := NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewRuntime(tr, RuntimeConfig{
		Role:           "edge",
		SigningKeyPath: path,
		SubmitterHost:  "studio",
		PlanID:         "plan-2", // configured for a different plan
	})
	if err == nil {
		t.Fatal("edge accepted a key minted for another plan")
	}
	if !strings.Contains(err.Error(), "plan-2") {
		t.Errorf("error should name the configured plan: %v", err)
	}
}

// Mutating a nil keyring must error rather than panic, on a path a signed
// submission can reach.
func TestNilKeyringMutatorsDoNotPanic(t *testing.T) {
	var ring *Keyring
	if err := ring.EndAuthority("k", testNow()); err == nil {
		t.Error("EndAuthority on a nil keyring should error")
	}
	if err := ring.Revoke("k", testNow()); err == nil {
		t.Error("Revoke on a nil keyring should error")
	}
	if err := ring.Renew("k", testNow(), DefaultLeaseConfig()); err == nil {
		t.Error("Renew on a nil keyring should error")
	}
}

func TestAdmitRequiresAKeyring(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))
	opts := h.admitOpts()
	opts.Verify.Keyring = nil
	if _, _, err := Admit(context.Background(), h.tr, object, opts); err == nil {
		t.Fatal("admission proceeded with no keyring")
	}
}

// --- Regressions for review 29d22093 ---

// An unconfigured hub must permit no spending. Treating "unset" as "unlimited"
// would make the one check between an autonomous session and real money fail
// open on a fresh install.
func TestUnconfiguredSpendPermitsNothing(t *testing.T) {
	h := newHarness(t)
	policy, err := NewPolicy("laptop", []string{"studio"}, 0) // no maximum configured
	if err != nil {
		t.Fatal(err)
	}
	key, _ := h.ring.Lookup("studio-test")
	key.SpendCeilingUSD = 0 // no grant either
	if err := h.ring.Add(key); err != nil {
		t.Fatal(err)
	}
	res := h.submit(jobRequest(WeftJobPayload{
		Command:           "echo hello",
		SpendCeilingUSD:   500.0,
		TargetConstraints: TargetConstraints{Hosts: []string{"studio"}},
	}))
	opts := h.admitOpts()
	opts.Policy = policy
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))

	adm, refusal, err := Admit(context.Background(), h.tr, object, opts)
	if err != nil {
		t.Fatal(err)
	}
	if adm != nil {
		t.Fatalf("an unconfigured hub authorized $%.2f of spending",
			adm.Authorization.EffectiveSpendCeilingUSD)
	}
	if refusal == nil || refusal.Code != ReasonOverSpendCeiling {
		t.Fatalf("want ReasonOverSpendCeiling, got %v", refusal)
	}
	if !strings.Contains(refusal.Detail, "permits nothing rather than everything") {
		t.Errorf("refusal should explain that unset means nothing: %q", refusal.Detail)
	}
}

// A job needing no money still runs under an unconfigured hub.
func TestZeroSpendSubmissionSurvivesAnUnconfiguredHub(t *testing.T) {
	h := newHarness(t)
	policy, err := NewPolicy("laptop", []string{"studio"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := h.ring.Lookup("studio-test")
	key.SpendCeilingUSD = 0
	if err := h.ring.Add(key); err != nil {
		t.Fatal(err)
	}
	res := h.submit(jobRequest(WeftJobPayload{
		Command:           "echo hello",
		TargetConstraints: TargetConstraints{Hosts: []string{"studio"}},
	}))
	opts := h.admitOpts()
	opts.Policy = policy
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))
	adm, refusal, err := Admit(context.Background(), h.tr, object, opts)
	if err != nil {
		t.Fatal(err)
	}
	if refusal != nil {
		t.Fatalf("a job requesting no spend was refused: %s", refusal.Detail)
	}
	if adm.Authorization.EffectiveSpendCeilingUSD != 0 {
		t.Errorf("effective ceiling = %v, want 0", adm.Authorization.EffectiveSpendCeilingUSD)
	}
}

// The hub exclusion must cover every name that reaches the hub, not just its
// hostname string.
func TestHubExclusionCoversLoopbackNames(t *testing.T) {
	for _, name := range []string{"localhost", "127.0.0.1", "::1", "LOCALHOST"} {
		if _, err := NewPolicy("laptop", []string{name}, 10.0); err == nil {
			t.Errorf("policy accepted %q as an execution target; it names the hub", name)
		}
	}
}

// Without the hub's own name the exclusion would compare against nothing.
func TestPolicyRefusesAnUnknownHubName(t *testing.T) {
	if _, err := NewPolicy("", []string{"studio"}, 10.0); err == nil {
		t.Fatal("policy was built without knowing the hub's own host name")
	}
}

func TestLoopbackTargetInASubmissionIsRefused(t *testing.T) {
	h := newHarness(t)
	res := h.submit(jobRequest(WeftJobPayload{
		Command:           "echo hello",
		SpendCeilingUSD:   1.0,
		TargetConstraints: TargetConstraints{Hosts: []string{"127.0.0.1"}},
	}))
	_, refusal := h.admitNonce(res.Nonce)
	if refusal == nil || refusal.Code != ReasonTargetNotAllowed {
		t.Fatalf("a submission targeting the loopback address was not refused: %v", refusal)
	}
}

// Ending a plan must survive a hub restart. An in-memory-only close would
// silently reopen the plan's authority on the next load.
func TestPlanEndPersistsAcrossReload(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	key, _ := h.ring.Lookup("studio-test")
	key.PlanID = "plan-7"
	if err := SaveKey(dir, key); err != nil {
		t.Fatal(err)
	}
	ring, err := LoadKeyring(dir)
	if err != nil {
		t.Fatal(err)
	}

	res := h.submit(planEndedRequest("plan-7"))
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))
	opts := h.admitOpts()
	opts.Verify.Keyring = ring
	if _, refusal, err := Admit(context.Background(), h.tr, object, opts); err != nil || refusal != nil {
		t.Fatalf("plan end failed: %v %v", err, refusal)
	}

	reloaded, err := LoadKeyring(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, ok := reloaded.Lookup("studio-test")
	if !ok {
		t.Fatal("key vanished")
	}
	if after.NotAfter.After(testNow()) {
		t.Fatalf("plan authority reopened after reload: not_after is %s", after.NotAfter)
	}
}

// Hub-side faults are errors, not verdicts on the submission.
func TestPlanEndedHubFaultIsAnErrorNotARefusal(t *testing.T) {
	h := newHarness(t)
	h.planKey("plan-7", 5.0)
	res := h.submit(planEndedRequest("plan-7"))
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))

	opts := h.admitOpts()
	opts.Verify.Keyring = nil
	_, refusal, err := Admit(context.Background(), h.tr, object, opts)
	if refusal != nil {
		t.Fatalf("a missing keyring is a hub fault, not a refusal: %s", refusal.Code)
	}
	if err == nil {
		t.Fatal("a missing keyring should surface as an error")
	}
}

// Plan-scoped failures get their own codes rather than borrowing the target
// allowlist's, so an operator is not sent to the wrong configuration.
func TestPlanEndedRefusalsUseTheirOwnCodes(t *testing.T) {
	h := newHarness(t)
	h.planKey("plan-7", 5.0)
	res := h.submit(planEndedRequest("plan-99"))
	_, refusal := h.admitNonce(res.Nonce)
	if refusal == nil || refusal.Code != ReasonPlanScopeMismatch {
		t.Fatalf("want ReasonPlanScopeMismatch, got %v", refusal)
	}

	h2 := newHarness(t)
	key, _ := h2.ring.Lookup("studio-test")
	key.PlanID = "" // bound to no plan
	if err := h2.ring.Add(key); err != nil {
		t.Fatal(err)
	}
	res2 := h2.submit(planEndedRequest("plan-7"))
	_, refusal2 := h2.admitNonce(res2.Nonce)
	if refusal2 == nil || refusal2.Code != ReasonKeyNotBoundToPlan {
		t.Fatalf("want ReasonKeyNotBoundToPlan, got %v", refusal2)
	}
}

// A kind whose payload is its identity collapses duplicates, even across
// different nonces. The nonce seen-set alone would let both through.
func TestContentIdempotentKindCollapsesDuplicates(t *testing.T) {
	h := newHarness(t)
	h.planKey("plan-7", 5.0)

	first := h.submit(planEndedRequest("plan-7"))
	if _, refusal := h.admitNonce(first.Nonce); refusal != nil {
		t.Fatalf("first submission refused: %s", refusal.Detail)
	}

	// A second submission: different nonce, identical content.
	second := h.submit(planEndedRequest("plan-7"))
	if second.Nonce == first.Nonce {
		t.Fatal("test needs two distinct nonces")
	}
	_, refusal := h.admitNonce(second.Nonce)
	if refusal == nil || refusal.Code != ReasonDuplicateContent {
		t.Fatalf("want ReasonDuplicateContent, got %v", refusal)
	}
}

// Job submissions are NOT content-idempotent: running the same command twice
// is a legitimate request, so two nonces must mean two jobs.
func TestJobSubmissionsAreNotCollapsedByContent(t *testing.T) {
	h := newHarness(t)
	first := h.submit(defaultRequest())
	if _, refusal := h.admitNonce(first.Nonce); refusal != nil {
		t.Fatalf("first job refused: %s", refusal.Detail)
	}
	second := h.submit(defaultRequest())
	if _, refusal := h.admitNonce(second.Nonce); refusal != nil {
		t.Fatalf("an identical second job was collapsed, but running the same command "+
			"twice is legitimate: %s", refusal.Detail)
	}
}

// An unreadable seen-set on the content path is unknown, not a duplicate.
func TestUnreadableSeenSetOnContentPathIsUnknown(t *testing.T) {
	h := newHarness(t)
	h.planKey("plan-7", 5.0)
	res := h.submit(planEndedRequest("plan-7"))
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))

	opts := h.admitOpts()
	opts.Seen = brokenSeenSet{}
	_, refusal, err := Admit(context.Background(), h.tr, object, opts)
	if refusal != nil {
		t.Fatalf("an unreadable seen-set must not be a terminal refusal: %s", refusal.Code)
	}
	if err == nil {
		t.Fatal("expected an error so the caller retries")
	}
}

// A kind that states a fact rather than requesting work can be exempted from
// staleness. Dropping such a record would be read downstream as the event never
// having happened — a silent absence standing in for a negative observation.
func TestNeverExpiresExemptsAKindFromStaleness(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))

	opts := h.admitOpts()
	opts.Verify.Kinds = NewKindRegistry()
	opts.Verify.Kinds.Register(KindWeftJobSubmission, KindPolicy{NeverExpires: true})
	opts.Verify.DefaultTTL = time.Minute
	opts.Verify.Now = testNow().Add(30 * 24 * time.Hour) // a month later

	if _, refusal, _ := Admit(context.Background(), h.tr, object, opts); refusal != nil {
		t.Fatalf("a never-expiring kind was refused as stale: %s", refusal.Code)
	}
}

// A zero TTL means "use the default", not "never expire", so a forgotten value
// fails toward a bound rather than toward none.
func TestZeroTTLFallsBackToTheDefaultRatherThanNoBound(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))

	opts := h.admitOpts()
	opts.Verify.Kinds = NewKindRegistry()
	opts.Verify.Kinds.Register(KindWeftJobSubmission, KindPolicy{}) // TTL unset
	opts.Verify.DefaultTTL = time.Minute
	opts.Verify.Now = testNow().Add(time.Hour)

	_, refusal, _ := Admit(context.Background(), h.tr, object, opts)
	if refusal == nil || refusal.Code != ReasonExpired {
		t.Fatalf("an unset kind TTL should fall back to the default bound, got %v", refusal)
	}
}

// The hub answers to more than one name. Checking only the system host name
// would leave the exclusion matching nothing an operator would plausibly write.
func TestHubNamesCoverShortAndLoopbackForms(t *testing.T) {
	names := HubNames("Olivers-MacBook.local")
	for _, want := range []string{"Olivers-MacBook.local", "Olivers-MacBook", "localhost", "127.0.0.1"} {
		if !containsFold(names, want) {
			t.Errorf("HubNames omitted %q: %v", want, names)
		}
	}
	for _, name := range []string{"Olivers-MacBook", "olivers-macbook", "Olivers-MacBook.local"} {
		if _, err := NewPolicy("Olivers-MacBook.local", []string{name}, 10.0); err == nil {
			t.Errorf("policy accepted %q, which names the hub", name)
		}
	}
	// A genuinely different host is still allowed.
	if _, err := NewPolicy("Olivers-MacBook.local", []string{"studio"}, 10.0); err != nil {
		t.Errorf("policy rejected a legitimate target: %v", err)
	}
}

// A nil *FileSeenSet in a SeenSet interface passes every `!= nil` guard, so the
// receiver has to defend itself.
func TestTypedNilSeenSetDoesNotPanic(t *testing.T) {
	var concrete *FileSeenSet
	var iface SeenSet = concrete
	if iface == nil {
		t.Fatal("test premise wrong: a typed nil should not compare equal to nil")
	}
	if _, err := iface.Seen("abc"); err == nil {
		t.Error("Seen on a typed-nil seen-set should error, not succeed")
	}
	if err := iface.Record("abc", testNow()); err == nil {
		t.Error("Record on a typed-nil seen-set should error, not succeed")
	}
}

// Losing the race to record a nonce means another process already admitted the
// submission. Swallowing the collision would admit it twice — for a rental,
// paying twice.
func TestLosingTheRecordRaceRefusesRatherThanAdmitting(t *testing.T) {
	h := newHarness(t)
	res := h.submit(defaultRequest())
	object, _ := h.tr.Get(context.Background(), InboxKey(res.Nonce))

	// Another process records the nonce between this one's check and its write.
	if err := h.seen.Record(res.Nonce, testNow()); err != nil {
		t.Fatal(err)
	}
	raced := &raceSeenSet{inner: h.seen}
	opts := h.admitOpts()
	opts.Seen = raced

	adm, refusal, err := Admit(context.Background(), h.tr, object, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if adm != nil {
		t.Fatal("a submission another process had already recorded was admitted again")
	}
	if refusal == nil || refusal.Code != ReasonReplayed {
		t.Fatalf("want ReasonReplayed, got %v", refusal)
	}
}

// raceSeenSet reports every nonce as unseen, so Record is reached even though
// the underlying set already holds it — the interleaving that a plain
// check-then-write cannot rule out.
type raceSeenSet struct{ inner *FileSeenSet }

func (r *raceSeenSet) Seen(string) (bool, error) { return false, nil }
func (r *raceSeenSet) Record(nonce string, at time.Time) error {
	return r.inner.Record(nonce, at)
}

// A negative grant authorizes nothing. It satisfies neither `> 0` nor `== 0`,
// so a guard written the obvious way falls through to the hub maximum.
func TestNegativeKeyCeilingAuthorizesNothing(t *testing.T) {
	h := newHarness(t)
	key, _ := h.ring.Lookup("studio-test")
	key.SpendCeilingUSD = -5
	if err := h.ring.Add(key); err == nil {
		t.Fatal("keyring accepted a negative spend ceiling")
	}

	// Even if one reaches Authorize by another route, it must not fail open.
	policy, err := NewPolicy("laptop", []string{"studio"}, 10.0)
	if err != nil {
		t.Fatal(err)
	}
	v := &VerifiedEnvelope{
		envelope:   Envelope{Nonce: "n"},
		keyID:      "k",
		host:       "studio",
		keyCeiling: -5,
	}
	auth, refusal := Authorize(v, &WeftJobPayload{Command: "x", SpendCeilingUSD: 9}, policy)
	if auth != nil {
		t.Fatalf("a negative grant authorized $%.2f", auth.EffectiveSpendCeilingUSD)
	}
	if refusal == nil || refusal.Code != ReasonOverSpendCeiling {
		t.Fatalf("want ReasonOverSpendCeiling, got %v", refusal)
	}
}
