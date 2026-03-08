package cloud

import (
	"errors"
	"time"
)

// ErrInstanceNotFound is returned by ShowInstance when the instance no longer
// exists in the provider's inventory.
var ErrInstanceNotFound = errors.New("instance not found")

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

	// DestroyInstance tears down an instance.
	DestroyInstance(instanceID string) error

	// CopyBetweenInstances copies files from one instance to another.
	// Uses provider-level copy (e.g., vastai copy) which is LAN-speed within a data center.
	CopyBetweenInstances(srcInstanceID, srcPath, dstInstanceID, dstPath string) error

	// WorkspacePath returns the default workspace path on instances (e.g., "/workspace/").
	WorkspacePath() string

	// SelfDestructCmd returns the shell command for an instance to destroy itself.
	// providerInstanceID is the provider-specific instance ID (e.g., Vast.ai instance number).
	SelfDestructCmd(providerInstanceID string) string
}
