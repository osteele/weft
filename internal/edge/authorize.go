package edge

import (
	"fmt"
	"slices"
	"strings"
)

// Policy is the hub's authorization configuration. It is hub-side state, never
// derived from a submission, so no envelope can widen it.
type Policy struct {
	// AllowedTargets are the execution targets an edge submission may run on.
	// The hub's own host must never appear here; NewPolicy enforces that.
	AllowedTargets []string
	// MaxSpendUSD is the hub's half of the ceiling on a single submission or
	// work-starting control. Authorization refuses a request declaring more
	// than the effective ceiling; it does not itself constrain later spending.
	MaxSpendUSD float64
	// AllowForeignJobControl widens control authority to jobs admitted by a
	// different signing key. The default is false, which confines each plan to
	// jobs admitted from its own key.
	AllowForeignJobControl bool
	// hubNames are every name that denotes this hub, all refused as targets.
	//
	// One name is not enough. os.Hostname() returns a system name like
	// "Olivers-MacBook.local", while weft's own host namespace uses short
	// inventory names, so a check against the system name alone would not
	// match the name an operator would actually write in an allowlist.
	hubNames []string
}

// JobControlProvenanceLookup returns the signing key recorded on a job row.
// found=false is confirmed absence; an error means the lookup is unknown and
// must not be turned into an authority refusal.
type JobControlProvenanceLookup func(jobID int64) (signingKeyID string, found bool, err error)

// NewPolicy builds a policy and rejects a configuration that would let edge
// work execute on the hub itself.
//
// This is checked at construction rather than at each authorization, so a
// misconfiguration fails when the hub starts rather than on the first
// submission that happens to exploit it.
func NewPolicy(hubHost string, allowedTargets []string, maxSpendUSD float64) (*Policy, error) {
	// An unknown hub name would silently disable the exclusion below, since
	// nothing compares equal to the empty string.
	if hubHost == "" {
		return nil, fmt.Errorf(
			"edge policy cannot be built without the hub's own host name; " +
				"without it the check that keeps edge work off the hub matches nothing")
	}
	names := HubNames(hubHost)
	for _, target := range allowedTargets {
		if isHubHost(target, names) {
			return nil, fmt.Errorf(
				"edge policy lists %q, which names the hub itself; "+
					"edge-submitted work must never execute on the hub", target)
		}
	}
	if maxSpendUSD < 0 {
		return nil, fmt.Errorf("edge policy max spend must not be negative, got %.2f", maxSpendUSD)
	}
	return &Policy{
		AllowedTargets: slices.Clone(allowedTargets),
		MaxSpendUSD:    maxSpendUSD,
		hubNames:       names,
	}, nil
}

// Authorization is the outcome of admitting a verified submission: what the hub
// decided, as distinct from who signed it.
type Authorization struct {
	// EffectiveSpendCeilingUSD is the lower of the requested ceiling and the
	// grant, and is what an admitted request is authorized to commit.
	//
	// The inbox poller carries this value into the durable job request before
	// the ordinary recording and placement path sees it.
	EffectiveSpendCeilingUSD float64
	// Targets is the set of execution targets this submission may use, after
	// intersecting the request with the allowlist.
	Targets []string
}

// Authorize decides whether verified work may run.
//
// It takes a *VerifiedEnvelope rather than an Envelope so that it is not
// possible to authorize unverified content: the type cannot be constructed
// outside this package. The payload must be one whose bytes hashed to the
// envelope's signed PayloadDigest, which is what makes its authority fields
// as non-forgeable as the envelope's own.
//
// A valid signature never causes a check here to be skipped. The signature
// establishes who wrote the request; every constraint in the request is still
// the writer's claim about what it wants, and a compromised edge signs whatever
// it likes with a key the hub accepts. These checks are what remain when that
// happens, so they must not be folded into verification even though the values
// they read are signed.
func Authorize(v *VerifiedEnvelope, payload *WeftJobPayload, policy *Policy) (*Authorization, *Refusal) {
	if policy == nil {
		return nil, refuse(ReasonTargetNotAllowed,
			"no edge policy is configured; refusing to admit submission %s", v.envelope.Nonce)
	}
	if payload == nil {
		return nil, refuse(ReasonMalformed,
			"submission %s has no payload to authorize", v.envelope.Nonce)
	}
	env := v.envelope

	// Execution targets. An empty request means "hub chooses", which is the
	// whole allowlist. A named request must be a subset of it.
	var targets []string
	if len(payload.TargetConstraints.Hosts) == 0 {
		targets = slices.Clone(policy.AllowedTargets)
		if len(targets) == 0 {
			return nil, refuse(ReasonTargetNotAllowed,
				"submission %s from %s requests hub-chosen placement but no execution targets are allowed",
				env.Nonce, v.host)
		}
	} else {
		for _, want := range payload.TargetConstraints.Hosts {
			if isHubHost(want, policy.hubNames) {
				return nil, refuse(ReasonTargetNotAllowed,
					"signature valid (key %s, host %s); target %q names the hub itself (%v) and is never an allowed execution target",
					v.keyID, v.host, want, policy.hubNames)
			}
			if !containsFold(policy.AllowedTargets, want) {
				return nil, refuse(ReasonTargetNotAllowed,
					"signature valid (key %s, host %s); target %q is not in the allowed execution targets %v",
					v.keyID, v.host, want, policy.AllowedTargets)
			}
			targets = append(targets, want)
		}
	}

	// Spend ceiling. The authoritative grant is hub-side — the key's ceiling
	// and the policy maximum — and is never transmitted. The payload may carry
	// a downward-only request, which is honored when smaller than the grant and
	// refused when larger, rather than silently clamped: clamping would run the
	// work under a budget its author did not choose.
	// The effective ceiling is the smaller of the hub maximum and the plan's
	// grant. Zero means NOTHING is permitted, not that everything is: an
	// unconfigured hub must refuse spending rather than authorize whatever the
	// edge asks for. Failing open here would defeat the one check standing
	// between an autonomous session and real money.
	// Anything that is not a positive grant authorizes nothing. Testing for
	// `> 0` rather than `== 0` matters: a negative value satisfies neither
	// `> 0` nor `== 0`, and would otherwise fall through to the full hub
	// maximum — failing open on the one path that spends money.
	effective, refusal := authorizeSpend(v, payload.SpendCeilingUSD, policy)
	if refusal != nil {
		return nil, refusal
	}

	return &Authorization{EffectiveSpendCeilingUSD: effective, Targets: targets}, nil
}

// AuthorizeJobControl decides whether a verified plan may control a job.
//
// Ownership is established from the hub's durable job-row provenance. A valid
// signature identifies the requester but never substitutes for this check.
func AuthorizeJobControl(v *VerifiedEnvelope, payload *WeftJobControlPayload, policy *Policy, lookup JobControlProvenanceLookup) (*Authorization, *Refusal, error) {
	if policy == nil {
		return nil, refuse(ReasonJobControlAuthority,
			"job-control authority check failed: no edge policy is configured for request %s", v.envelope.Nonce), nil
	}
	if payload == nil {
		return nil, refuse(ReasonMalformed,
			"submission %s has no job-control payload to authorize", v.envelope.Nonce), nil
	}
	if lookup == nil {
		return nil, nil, fmt.Errorf("job-control authority for %s is unknown: no provenance lookup is configured", v.envelope.Nonce)
	}
	ownerKey, found, err := lookup(payload.JobID)
	if err != nil {
		return nil, nil, fmt.Errorf("job-control authority for job %d is unknown: %w", payload.JobID, err)
	}
	if !found {
		return nil, refuse(ReasonJobControlAuthority,
			"job-control authority check failed: job %d is not present on this hub", payload.JobID), nil
	}
	if !policy.AllowForeignJobControl && ownerKey != v.keyID {
		owner := ownerKey
		if owner == "" {
			owner = "a hub-local submission"
		}
		return nil, refuse(ReasonJobControlAuthority,
			"job-control authority check failed: signature valid (key %s, plan %s), but job %d belongs to %s",
			v.keyID, v.planLabel(), payload.JobID, owner), nil
	}

	auth := &Authorization{}
	if payload.Action == ControlRestart || payload.Action == ControlEdit {
		effective, refusal := authorizeSpend(v, payload.SpendCeilingUSD, policy)
		if refusal != nil {
			return nil, refusal, nil
		}
		auth.EffectiveSpendCeilingUSD = effective
	}
	return auth, nil, nil
}

func authorizeSpend(v *VerifiedEnvelope, requested float64, policy *Policy) (float64, *Refusal) {
	ceiling := policy.MaxSpendUSD
	if policy.MaxSpendUSD <= 0 || v.keyCeiling <= 0 {
		ceiling = 0
	} else if v.keyCeiling < ceiling {
		ceiling = v.keyCeiling
	}
	if requested < 0 {
		return 0, refuse(ReasonOverSpendCeiling,
			"submission %s declares a negative spend ceiling %.2f", v.envelope.Nonce, requested)
	}
	if requested > ceiling {
		if ceiling == 0 {
			return 0, refuse(ReasonOverSpendCeiling,
				"signature valid (key %s, host %s); this submission requests $%.2f but no spend is "+
					"authorized: the hub's max_spend_usd and plan %s's grant are both unset, "+
					"which permits nothing rather than everything",
				v.keyID, v.host, requested, v.planLabel())
		}
		return 0, refuse(ReasonOverSpendCeiling,
			"signature valid (key %s, host %s); requested spend ceiling $%.2f exceeds the $%.2f granted to plan %s",
			v.keyID, v.host, requested, ceiling, v.planLabel())
	}
	if requested == 0 {
		return ceiling, nil
	}
	return requested, nil
}

func containsFold(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}

func (v *VerifiedEnvelope) planLabel() string {
	if v.planID == "" {
		return "(unnamed)"
	}
	return v.planID
}

// HubNames expands a system host name into every name that denotes this hub.
//
// os.Hostname() returns something like "Olivers-MacBook.local", but weft's host
// namespace uses short inventory names, and loopback names reach the hub too.
// Checking only the system name would leave the hub-exclusion guarantee
// matching nothing an operator would plausibly write.
func HubNames(hubHost string) []string {
	names := []string{"localhost", "127.0.0.1", "::1"}
	hubHost = strings.TrimSpace(hubHost)
	if hubHost != "" {
		names = append(names, hubHost)
		// The short form: "Olivers-MacBook.local" also answers to
		// "Olivers-MacBook", which is the shape a weft host file would use.
		if short, _, found := strings.Cut(hubHost, "."); found && short != "" {
			names = append(names, short)
		}
	}
	return names
}

// The predicate is inlined rather than borrowed from internal/sync. Importing
// that package for four lines pulled internal/config, internal/db, internal/ssh
// and a dozen more into this one transitively, which would make the import
// cycle RuntimeConfig exists to avoid a real cycle the moment any of them
// needed to reference internal/edge.
func isHubHost(target string, hubNames []string) bool {
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" {
		return true
	}
	for _, name := range hubNames {
		if strings.EqualFold(target, name) {
			return true
		}
	}
	return false
}
