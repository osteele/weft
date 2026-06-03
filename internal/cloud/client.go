package cloud

import (
	"errors"
	"strings"
	"time"
)

// ErrInstanceNotFound is returned by ShowInstance when the instance no longer
// exists in the provider's inventory.
var ErrInstanceNotFound = errors.New("instance not found")

// ErrOfferUnavailable is returned when a previously discovered provider offer
// disappears before instance creation succeeds.
var ErrOfferUnavailable = errors.New("offer unavailable")

// ErrProviderRejected is returned when the provider accepts a request but
// responds with a failure (e.g. machine busy, success=false, API 400 with a
// payload on stderr). For instance creation the remedy is to try a different
// offer — `isRetryableCreateError` / `IsRetryableNewInstanceLaunchError`
// branch on this sentinel. The error text is intentionally operation-neutral
// because the same sentinel wraps generic CLI failures from non-create paths
// (destroy, show, search) where "instance creation" would be misleading.
var ErrProviderRejected = errors.New("provider rejected request")

// ErrAccountCreditExhausted is returned when a provider request fails because
// the authenticated account has insufficient credit/balance. Unlike
// ErrProviderRejected, retrying against another offer will not help — the
// operator needs to top up before any further placement attempts succeed.
// Schedulers should treat this as a provider-wide outage and stop attempting
// new instance creations on the affected provider until credit is restored.
var ErrAccountCreditExhausted = errors.New("account credit exhausted")

// ErrProviderCommandTimeout is returned when a provider CLI/API command exceeds
// weft's local timeout before returning a provider response.
var ErrProviderCommandTimeout = errors.New("provider command timed out")

// ProjectRootDir is the default root directory used for synced project trees on
// cloud instances.
const ProjectRootDir = "/workspace"

// ProviderError is a structured wrapper around an upstream provider failure.
// It carries enough context for error-class coalescing (Fingerprint) without
// breaking the sentinel-based retry classification that already runs through
// the codebase: errors.Is(err, ErrProviderRejected) and peers continue to
// match because ProviderError.Unwrap() returns the embedded Sentinel.
//
// Callers that need the structured fields use errors.As(err, &pe). Callers
// that only care about retry policy continue to use errors.Is.
//
// Fingerprint is a stable, low-cardinality coalescing key with the shape
// "<provider>/<op>/<class>[:<key>]" — e.g.
//   - vastai/search-offers/400/bad-field:driver_vers
//   - vastai/create-instance/account-credit-exhausted
//   - vastai/cli/timeout
//
// The intent: many jobs hitting the same systemic upstream failure should
// coalesce into a single incident in display surfaces.
type ProviderError struct {
	Sentinel    error  // one of the Err* sentinels above
	Provider    string // e.g. "vastai", "runpod"
	Op          string // e.g. "search-offers", "create-instance"
	StatusCode  int    // upstream HTTP status when known; 0 = unknown
	Message     string // upstream-provided human text
	Fingerprint string // stable coalescing key; empty when not classified
}

// Error renders the wrapped error with the actionable message FIRST so
// single-line truncation in compact display surfaces preserves the part
// users need to act on. Shape:
//
//	"<message>: <sentinel> (<provider> <op>)"
//
// — e.g.
//
//	"ask_contract_offers.driver_vers gte None…: provider rejected request (vastai search-offers)"
//
// When the message is empty, falls back to:
//
//	"<sentinel> (<provider> <op>)"
func (e *ProviderError) Error() string {
	if e == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if msg := strings.TrimSpace(e.Message); msg != "" {
		parts = append(parts, msg)
	}
	if e.Sentinel != nil {
		parts = append(parts, e.Sentinel.Error())
	}
	out := strings.Join(parts, ": ")
	tag := ""
	switch {
	case e.Provider != "" && e.Op != "":
		tag = " (" + e.Provider + " " + e.Op + ")"
	case e.Op != "":
		tag = " (" + e.Op + ")"
	case e.Provider != "":
		tag = " (" + e.Provider + ")"
	}
	return out + tag
}

func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Sentinel
}

// Is preserves the existing sentinel-based dispatch used throughout the
// codebase. It returns true when the embedded Sentinel matches the target
// (directly or via further Unwrap chains).
func (e *ProviderError) Is(target error) bool {
	if e == nil {
		return target == nil
	}
	if target == nil {
		return false
	}
	return errors.Is(e.Sentinel, target)
}

// FingerprintOf returns the coalescing fingerprint embedded in err, or "" when
// err is not a *ProviderError. Safe to call on a nil error.
func FingerprintOf(err error) string {
	if err == nil {
		return ""
	}
	var pe *ProviderError
	if errors.As(err, &pe) && pe != nil {
		return pe.Fingerprint
	}
	return ""
}

// AsProviderError unwraps err to its first *ProviderError, returning (nil, false)
// when none is present. Convenience wrapper for the common errors.As call site.
func AsProviderError(err error) (*ProviderError, bool) {
	if err == nil {
		return nil, false
	}
	var pe *ProviderError
	if errors.As(err, &pe) && pe != nil {
		return pe, true
	}
	return nil, false
}

// WrapProviderError builds a *ProviderError around an existing sentinel and
// detail. fingerprintParts are joined as "p1/p2/..." with a trailing
// ":<key>" when key is non-empty. Empty parts are skipped.
func WrapProviderError(sentinel error, provider, op string, statusCode int, message string, fingerprintParts ...string) *ProviderError {
	return &ProviderError{
		Sentinel:    sentinel,
		Provider:    provider,
		Op:          op,
		StatusCode:  statusCode,
		Message:     strings.TrimSpace(message),
		Fingerprint: BuildFingerprint(fingerprintParts...),
	}
}

// BuildFingerprint joins non-empty parts with "/" to produce a stable
// coalescing key. The convention is "<provider>/<op>/<class>[:<key>]"; the
// last part may carry a ":<key>" suffix.
func BuildFingerprint(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "/")
}

var _ error = (*ProviderError)(nil)

// Client is the provider-neutral interface for interacting with cloud GPU providers.
type Client interface {
	// Provider returns which cloud provider this client connects to.
	Provider() Provider

	// Available checks that the provider's CLI/API is installed and authenticated.
	Available() error

	// SearchOffers queries available GPU offers matching constraints.
	SearchOffers(constraints OfferConstraints) ([]Offer, error)

	// CreateInstance creates a new instance from an offer.
	// offerID is the provider-specific offer identifier (string).
	CreateInstance(offerID string, opts CreateOpts) (*Instance, error)

	// ShowInstance fetches the current state of an instance.
	ShowInstance(instanceID string) (*Instance, error)

	// WaitReady polls until an instance reaches "running" status or the timeout expires.
	WaitReady(instanceID string, timeout time.Duration) (*Instance, error)

	// ListAllInstances returns all instances from the provider (for orphan detection).
	ListAllInstances() ([]Instance, error)

	// DestroyInstance tears down an instance.
	DestroyInstance(instanceID string) error

	// CopyBetweenInstances copies files from one instance to another.
	// Uses provider-level copy (e.g., vastai copy) which is LAN-speed within a data center.
	CopyBetweenInstances(srcInstanceID, srcPath, dstInstanceID, dstPath string) error

	// SelfDestructCmd returns the shell command for an instance to destroy itself.
	// providerInstanceID is the provider-specific instance ID (e.g., Vast.ai instance number).
	SelfDestructCmd(providerInstanceID string) string
}

// ProgressClient is an optional extension for providers that can surface
// provider-side instance-creation milestones for logging/UI display.
type ProgressClient interface {
	CreateInstanceWithProgress(offerID string, opts CreateOpts, progress ProgressFunc) (*Instance, error)
}
