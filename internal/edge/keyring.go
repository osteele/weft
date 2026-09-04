package edge

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Key is one plan execution's public key.
//
// A key is minted when a plan is handed to a host, and its validity window is
// short and renewed while the hub can see the plan still executing. Authority
// is a lease rather than a grant, so the default outcome of nobody doing
// anything is expiry: an abandoned plan, a crashed session, or a handoff nobody
// finished loses the ability to spend money within one window, instead of
// holding an open credential until a date nobody is watching.
//
// That also makes the keyring the single fact both systems agree on without
// federating any state. A session that keeps running after its tree went home
// is refused because renewal stopped, which the edge itself has no way to know.
//
// Keyring rows are append-only. A window that closed months ago is not a reason
// to delete anything: a record signed inside it must stay verifiable, or it
// degrades from evidence to assertion. Tidying the keyring is exactly the
// good-faith cleanup that would do that.
//
// The key also carries the authorization granted to that plan. SpendCeilingUSD
// lives here rather than in any submission because it is a property of the
// authorization, not a claim the submitter gets to make — see decision 0027.
type Key struct {
	KeyID     string `json:"key_id"`
	Host      string `json:"host"`
	PublicKey string `json:"public_key"`
	// PlanID names the plan execution this key was minted for.
	PlanID string `json:"plan_id,omitempty"`
	// Project is the project whose ownership window is intended to bound
	// renewal, at project granularity because that is what the hub can observe.
	//
	// No code reads this yet: renewal is manual, and the loop that would gate
	// on project ownership is not built. It is carried so that loop has the
	// association when it arrives, and the protocol doc marks the gate as
	// specified-not-implemented rather than describing it as a live control.
	Project   string    `json:"project,omitempty"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	// SpendCeilingUSD is the most this plan may spend. Hub-side, set at mint
	// time, never transmitted by the edge and therefore unforgeable by
	// construction rather than by cryptography.
	SpendCeilingUSD float64 `json:"spend_ceiling_usd,omitempty"`

	// RevokedAt marks the key as believed compromised, which repudiates its
	// past signatures as well as refusing new ones.
	//
	// Ending a plan normally does NOT use this field. Sliding NotAfter back to
	// the current time refuses new submissions while leaving everything already
	// signed admissible, which works only because validity is judged against
	// the envelope's SubmittedAt rather than the hub's processing clock. That
	// gives retirement and revocation genuinely different meanings out of one
	// field plus one flag:
	//
	//   not_after moved back — bounds ADMISSION. Nothing new; past work stands.
	//   revoked_at set       — repudiates EVIDENCE. Past signatures stop
	//                          proving anything.
	//
	// Retiring a plan with RevokedAt would retroactively repudiate every
	// submission it legitimately made, which matters most for callers whose
	// records are long-lived evidence rather than transient work.
	RevokedAt *time.Time `json:"revoked_at,omitempty"`

	Comment string `json:"comment,omitempty"`
}

func (k Key) parsePublic() (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(k.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("key %s: public key is not valid base64: %w", k.KeyID, err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("key %s: public key is %d bytes, want %d",
			k.KeyID, len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// usableAt reports whether the key may admit a submission signed at signedAt,
// given that the hub is deciding now.
//
// Validity is judged against the moment of signing, not the moment of
// processing, so a submission made while a key was valid still verifies when
// the hub gets to it after expiry.
//
// The two terminal states differ in what they take as their subject.
// Revocation repudiates past signatures, because a compromised key's earlier
// output is no longer evidence. Retirement bounds future admission only: a
// submission signed while the plan owned the tree stays admissible, and one
// signed after handback does not.
func (k Key) usableAt(signedAt time.Time) *Refusal {
	if k.RevokedAt != nil {
		return refuse(ReasonKeyRevoked,
			"key %s for host %s was revoked at %s; submissions signed with it are no longer accepted, including earlier ones",
			k.KeyID, k.Host, k.RevokedAt.UTC().Format(time.RFC3339))
	}
	if !k.NotBefore.IsZero() && signedAt.Before(k.NotBefore) {
		return refuse(ReasonKeyNotYetValid,
			"key %s is not valid until %s but the submission is dated %s",
			k.KeyID, k.NotBefore.UTC().Format(time.RFC3339), signedAt.UTC().Format(time.RFC3339))
	}
	if !k.NotAfter.IsZero() && signedAt.After(k.NotAfter) {
		// Phrased as a lapsed lease rather than a malformed request. An
		// autonomous submitter reading a generic rejection retries forever;
		// one told its authority ended reports a finished plan instead.
		return refuse(ReasonKeyExpired,
			"key %s for plan %s has a validity window that ended at %s, and the submission is dated %s; "+
				"the hub stopped renewing this plan's authority — this is not a problem with the submission",
			k.KeyID, k.planLabel(), k.NotAfter.UTC().Format(time.RFC3339),
			signedAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// Keyring holds the public keys a hub will accept submissions from.
type Keyring struct {
	keys map[string]Key
	// dir is where this keyring was loaded from. When set, mutations persist
	// immediately: a caller that had to remember a separate save step would
	// eventually forget one, and a key whose authority was closed in memory
	// only reopens on the next restart.
	dir string
	// Problems records key files that could not be loaded. They are skipped
	// rather than fatal, so one malformed row cannot take the whole keyring
	// offline — including the commands an operator would use to diagnose it.
	Problems []string
}

func NewKeyring(keys ...Key) *Keyring {
	ring := &Keyring{keys: make(map[string]Key, len(keys))}
	for _, k := range keys {
		ring.keys[k.KeyID] = k
	}
	return ring
}

// Lookup returns the key with the given id.
func (r *Keyring) Lookup(keyID string) (Key, bool) {
	if r == nil {
		return Key{}, false
	}
	k, ok := r.keys[keyID]
	return k, ok
}

// List returns all keys ordered by id.
func (r *Keyring) List() []Key {
	if r == nil {
		return nil
	}
	out := make([]Key, 0, len(r.keys))
	for _, k := range r.keys {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].KeyID < out[j].KeyID })
	return out
}

// Add inserts or replaces a key, validating it first.
//
// This is the only validator, so every write path must go through it rather
// than calling SaveKey directly.
func (r *Keyring) Add(k Key) error {
	if k.KeyID == "" {
		return fmt.Errorf("key id is required")
	}
	if k.Host == "" {
		return fmt.Errorf("key %s: host is required", k.KeyID)
	}
	if _, err := k.parsePublic(); err != nil {
		return err
	}
	if k.SpendCeilingUSD < 0 {
		return fmt.Errorf("key %s: spend ceiling must not be negative, got %.2f",
			k.KeyID, k.SpendCeilingUSD)
	}
	if r.keys == nil {
		r.keys = map[string]Key{}
	}
	// Revocation is a statement that the key is compromised, and re-registering
	// must not quietly undo it. Renew refuses a revoked key for the same
	// reason; without this, registration would be a way around that.
	if existing, ok := r.keys[k.KeyID]; ok && existing.RevokedAt != nil && k.RevokedAt == nil {
		return fmt.Errorf(
			"key %s was revoked at %s as compromised and cannot be re-registered; "+
				"mint a new key with a different id instead",
			k.KeyID, existing.RevokedAt.UTC().Format(time.RFC3339))
	}
	r.keys[k.KeyID] = k
	return nil
}

func (k Key) planLabel() string {
	if k.PlanID == "" {
		return "(unnamed)"
	}
	return k.PlanID
}

// Revoke marks a key compromised, repudiating its past signatures as well as
// refusing new ones. This is not how a plan ends; see Retire.
//
// Revocation is not deletion: the entry is retained so that a later submission
// signed by it is refused with ReasonKeyRevoked rather than the less
// informative ReasonUnknownKey.
func (r *Keyring) Revoke(keyID string, at time.Time) error {
	if r == nil {
		return fmt.Errorf("no keyring")
	}
	k, ok := r.keys[keyID]
	if !ok {
		return fmt.Errorf("no key with id %q", keyID)
	}
	if k.RevokedAt != nil {
		return fmt.Errorf("key %s was already revoked at %s",
			keyID, k.RevokedAt.UTC().Format(time.RFC3339))
	}
	k.RevokedAt = &at
	r.keys[keyID] = k
	return r.persist(k)
}

// persist writes a mutated key back to the directory the keyring came from.
// A keyring with no directory (a test ring, or one built in memory) is a no-op.
func (r *Keyring) persist(k Key) error {
	if r.dir == "" {
		return nil
	}
	return SaveKey(r.dir, k)
}

// LoadKeyring reads a keyring from a directory of one JSON file per key.
// A missing directory is an empty keyring, not an error: a hub with no edges
// configured is a normal state.
func LoadKeyring(dir string) (*Keyring, error) {
	ring := NewKeyring()
	ring.dir = dir
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return ring, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read keyring directory %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read key file %s: %w", path, err)
		}
		var k Key
		if err := json.Unmarshal(data, &k); err != nil {
			ring.Problems = append(ring.Problems, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if err := ring.Add(k); err != nil {
			ring.Problems = append(ring.Problems, fmt.Sprintf("%s: %v", path, err))
			continue
		}
	}
	return ring, nil
}

// SaveKey writes one key file into the keyring directory.
func SaveKey(dir string, k Key) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create keyring directory %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return fmt.Errorf("encode key %s: %w", k.KeyID, err)
	}
	data = append(data, '\n')
	path := filepath.Join(dir, k.KeyID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write key file %s: %w", path, err)
	}
	return nil
}

// LeaseConfig governs how a plan's submission authority is renewed.
//
// The window's lower bound is set by clock skew, not by security. Verification
// refuses a submission dated in the future and bounds staleness by TTL, so with
// hour-scale windows a few minutes of drift between hosts is harmless, while
// with minute-scale windows it becomes a refusal storm. Shortening the window
// past that floor buys nothing and breaks working submissions.
type LeaseConfig struct {
	// Window is how long a renewal grants authority for.
	Window time.Duration
	// Interval is how often the hub renews while it can see the plan running.
	Interval time.Duration
}

// MinLeaseWindow is the clock-skew floor described on LeaseConfig.
const MinLeaseWindow = time.Hour

// DefaultLeaseConfig is a six-hour window renewed hourly, so a plan survives
// three consecutive missed renewals before its authority lapses.
func DefaultLeaseConfig() LeaseConfig {
	return LeaseConfig{Window: 6 * time.Hour, Interval: time.Hour}
}

// Validate rejects a configuration that would make renewal fragile.
func (c LeaseConfig) Validate() error {
	if c.Window < MinLeaseWindow {
		return fmt.Errorf(
			"edge lease window %s is below the %s clock-skew floor; "+
				"shorter windows cause spurious refusals from ordinary host drift",
			c.Window, MinLeaseWindow)
	}
	if c.Interval <= 0 {
		return fmt.Errorf("edge lease renewal interval must be positive, got %s", c.Interval)
	}
	if c.Interval >= c.Window {
		return fmt.Errorf(
			"edge lease renewal interval %s must be shorter than the window %s, "+
				"or a single missed renewal ends the plan's authority",
			c.Interval, c.Window)
	}
	return nil
}

// Renew slides a key's admission window forward.
//
// The hub calls this only while it can see the plan is still executing. It
// needs no contact with the edge: verification is hub-side, so the hub moves
// the window unilaterally and the edge never learns the window exists.
//
// Renewal refuses a revoked key, because revocation is a statement about the
// key itself that renewal must not quietly undo.
func (r *Keyring) Renew(keyID string, now time.Time, cfg LeaseConfig) error {
	if r == nil {
		return fmt.Errorf("no keyring")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	k, ok := r.keys[keyID]
	if !ok {
		return fmt.Errorf("no key with id %q", keyID)
	}
	if k.RevokedAt != nil {
		return fmt.Errorf("key %s was revoked at %s and cannot be renewed",
			keyID, k.RevokedAt.UTC().Format(time.RFC3339))
	}
	// A lapsed key is not renewed, it is re-minted. Extending NotAfter past a
	// closed window does not merely resume admission from now: validity is
	// judged against the envelope's SubmittedAt, so it also makes everything
	// signed during the gap admissible again. Whatever caused the lapse — a
	// returned tree, a stopped renewal loop, an abandoned plan — is a decision
	// that must be made again deliberately rather than undone by a timer.
	if !k.NotAfter.IsZero() && now.After(k.NotAfter) {
		return fmt.Errorf(
			"key %s for plan %s lapsed at %s and cannot be renewed; renewing would also "+
				"readmit everything it signed during the gap. Mint a new key for the plan instead",
			keyID, k.planLabel(), k.NotAfter.UTC().Format(time.RFC3339))
	}
	k.NotAfter = now.Add(cfg.Window)
	r.keys[keyID] = k
	return r.persist(k)
}

// EndAuthority stops new submissions from a plan without repudiating any it
// already made, by moving the admission window's end to now.
//
// This is how a plan ends normally. Use Revoke only when the key is believed
// compromised and its past signatures should stop being treated as evidence.
func (r *Keyring) EndAuthority(keyID string, now time.Time) error {
	if r == nil {
		return fmt.Errorf("no keyring")
	}
	k, ok := r.keys[keyID]
	if !ok {
		return fmt.Errorf("no key with id %q", keyID)
	}
	// Only ever narrow. Assigning unconditionally would WIDEN the window on a
	// key that has already lapsed, and because validity is judged against the
	// envelope's SubmittedAt rather than the hub's clock, widening
	// retroactively admits every submission signed during the lapse — each
	// carrying the plan's spend grant. A function whose purpose is to close
	// authority must never be able to open it.
	if k.NotAfter.IsZero() || now.Before(k.NotAfter) {
		k.NotAfter = now
	}
	r.keys[keyID] = k
	return r.persist(k)
}
