package cloud

import (
	"errors"
	"time"
)

// ErrInstanceNotFound is returned by ShowInstance when the instance no longer
// exists in the provider's inventory.
var ErrInstanceNotFound = errors.New("instance not found")

// ErrOfferUnavailable is returned when a previously discovered provider offer
// disappears before instance creation succeeds.
var ErrOfferUnavailable = errors.New("offer unavailable")

// ErrProviderRejected is returned when the provider accepts the create request
// but responds with success=false (e.g. machine busy, provider-side failure).
// Like ErrOfferUnavailable, the remedy is to try a different offer.
var ErrProviderRejected = errors.New("provider rejected instance creation")

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
