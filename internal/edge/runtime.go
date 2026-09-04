package edge

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Runtime is the assembled edge or hub, built from configuration.
type Runtime struct {
	Role      string
	Transport Transport
	// Edge side.
	Signer        *Signer
	SubmitterHost string
	// Hub side.
	Keyring *Keyring
	Policy  *Policy
	// Seen is the interface, not the concrete type: assigning a nil
	// *FileSeenSet to an interface field produces a non-nil interface holding
	// a nil pointer, which passes every `!= nil` guard downstream.
	Seen  SeenSet
	Kinds *KindRegistry
	Lease LeaseConfig
}

// RuntimeConfig is the subset of weft configuration this package needs, kept
// as a plain struct so internal/edge does not import internal/config and
// create a cycle.
type RuntimeConfig struct {
	Role             string
	SigningKeyPath   string
	PlanID           string
	SubmitterHost    string
	KeyringDir       string
	SeenDir          string
	AllowedTargets   []string
	MaxSpendUSD      float64
	HubHost          string
	LeaseWindowHours float64
}

func (c RuntimeConfig) IsEdge() bool { return strings.EqualFold(c.Role, "edge") }

// NewRuntime assembles the pieces for this installation's role.
//
// An edge needs a signing key and nothing else; a hub needs a keyring, a
// policy, and a seen-set. Building the wrong half is an error rather than a
// silent no-op, so a misconfigured role fails at startup.
func NewRuntime(t Transport, cfg RuntimeConfig) (*Runtime, error) {
	rt := &Runtime{Role: "hub", Transport: t, Kinds: DefaultKindRegistry()}
	if cfg.IsEdge() {
		rt.Role = "edge"
	}

	if rt.Role == "edge" {
		if cfg.SigningKeyPath == "" {
			return nil, fmt.Errorf("edge role requires signing_key_dir and plan_id in [edge] config")
		}
		if cfg.SubmitterHost == "" {
			return nil, fmt.Errorf("edge role requires submitter_host in [edge] config")
		}
		signer, err := LoadSigner(cfg.SigningKeyPath)
		if err != nil {
			return nil, err
		}
		// The key id encodes the plan it was minted for, and the hub derives
		// the same id independently. A mismatch means this edge would sign
		// under another plan's identity and spend grant, so it fails loudly
		// rather than producing an "unknown key" refusal on the hub with
		// nothing pointing at the cause.
		if cfg.PlanID != "" {
			want := KeyIDFor(cfg.SubmitterHost, cfg.PlanID)
			if signer.KeyID() != want {
				return nil, fmt.Errorf(
					"signing key at %s has key id %q but this edge is configured for plan %q, "+
						"whose key id is %q.\nMint the plan's own key with "+
						"`weft edge key mint --plan %s` rather than reusing another plan's",
					cfg.SigningKeyPath, signer.KeyID(), cfg.PlanID, want, cfg.PlanID)
			}
		}
		rt.Signer = signer
		rt.SubmitterHost = cfg.SubmitterHost
		return rt, nil
	}

	ring, err := LoadKeyring(cfg.KeyringDir)
	if err != nil {
		return nil, err
	}
	rt.Keyring = ring

	policy, err := NewPolicy(cfg.HubHost, cfg.AllowedTargets, cfg.MaxSpendUSD)
	if err != nil {
		return nil, err
	}
	rt.Policy = policy

	if cfg.SeenDir != "" {
		seen, err := NewFileSeenSet(cfg.SeenDir)
		if err != nil {
			return nil, err
		}
		rt.Seen = seen
	}

	rt.Lease = DefaultLeaseConfig()
	if cfg.LeaseWindowHours > 0 {
		rt.Lease.Window = time.Duration(cfg.LeaseWindowHours * float64(time.Hour))
	}
	if err := rt.Lease.Validate(); err != nil {
		return nil, err
	}
	return rt, nil
}

// GuardSigningKeyPath refuses a path inside a version-controlled tree.
//
// A private key written into a project tree lands in that repository's history
// on every machine the tree reaches, and sync exclusions do not help because a
// tracked file is not excluded. The failure is silent and permanent, so it is
// checked rather than documented.
func GuardSigningKeyPath(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve signing key path: %w", err)
	}
	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		for _, vcs := range []string{".git", ".jj"} {
			if _, err := os.Stat(filepath.Join(dir, vcs)); err == nil {
				return fmt.Errorf(
					"refusing to write a signing key at %s: it is inside the %s repository at %s.\n"+
						"A key in a version-controlled tree enters history on every machine that tree "+
						"reaches, and sync excludes do not help because a tracked file is not excluded.\n"+
						"Put it outside the tree, for example ~/.config/weft/edge-keys/.",
					abs, vcs, dir)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
	}
}

// KeyIDFor derives a key id from the host and plan.
//
// Both sides compute this independently — the edge when minting, the hub when
// registering — so they agree without transmitting the id. That only holds if
// neither side can substitute a different plan's key, which is why the edge
// validates its loaded key against its configured plan.
func KeyIDFor(host, plan string) string {
	return fmt.Sprintf("%s-%s", host, plan)
}
