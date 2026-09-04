package edge

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Signer holds an edge's private key. It exists only on the edge.
type Signer struct {
	keyID   string
	private ed25519.PrivateKey
}

// NewSigner builds a signer from a raw Ed25519 private key.
func NewSigner(keyID string, private ed25519.PrivateKey) (*Signer, error) {
	if keyID == "" {
		return nil, fmt.Errorf("key id is required")
	}
	if len(private) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key is %d bytes, want %d", len(private), ed25519.PrivateKeySize)
	}
	return &Signer{keyID: keyID, private: private}, nil
}

// GenerateSigner creates a new keypair and returns the signer together with the
// public Key to be installed on the hub.
func GenerateSigner(keyID, host string) (*Signer, Key, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, Key{}, fmt.Errorf("generate ed25519 keypair: %w", err)
	}
	signer, err := NewSigner(keyID, priv)
	if err != nil {
		return nil, Key{}, err
	}
	return signer, Key{
		KeyID:     keyID,
		Host:      host,
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	}, nil
}

func (s *Signer) KeyID() string { return s.keyID }

// PublicKey returns the base64 public half, for installation on a hub.
func (s *Signer) PublicKey() string {
	return base64.StdEncoding.EncodeToString(s.private.Public().(ed25519.PublicKey))
}

// Sign serializes the envelope once and signs those exact octets.
//
// The returned object embeds the serialization that was signed. Nothing
// re-serializes it afterwards, on either side, which is why the protocol needs
// no canonical encoding rules.
func (s *Signer) Sign(env Envelope) ([]byte, error) {
	if env.ProtocolVersion == 0 {
		env.ProtocolVersion = ProtocolVersion
	}
	octets, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("encode envelope: %w", err)
	}
	signature := ed25519.Sign(s.private, octets)
	return encodeFrame(s.keyID, signature, octets), nil
}

// LoadSigner reads a private key file written by SaveSigner.
func LoadSigner(path string) (*Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read edge signing key %s: %w", path, err)
	}
	var stored storedSigner
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("parse edge signing key %s: %w", path, err)
	}
	raw, err := base64.StdEncoding.DecodeString(stored.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("edge signing key %s: private key is not valid base64: %w", path, err)
	}
	return NewSigner(stored.KeyID, ed25519.PrivateKey(raw))
}

type storedSigner struct {
	KeyID      string `json:"key_id"`
	Host       string `json:"host"`
	PrivateKey string `json:"private_key"`
}

// SaveSigner writes the private key with owner-only permissions.
func SaveSigner(path string, s *Signer, host string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create key directory: %w", err)
	}
	data, err := json.MarshalIndent(storedSigner{
		KeyID:      s.keyID,
		Host:       host,
		PrivateKey: base64.StdEncoding.EncodeToString(s.private),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode edge signing key: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write edge signing key %s: %w", path, err)
	}
	return nil
}
