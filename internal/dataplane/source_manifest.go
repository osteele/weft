package dataplane

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// SourceManifestRoot contains the fields that define one source root's
// identity. Field order and JSON names are part of the manifest hash protocol.
type SourceManifestRoot struct {
	MountBasename string       `json:"mount_basename"`
	Hash          string       `json:"hash"`
	R2Key         string       `json:"r2_key"`
	Blobs         []SourceBlob `json:"blobs,omitempty"`
}

// SourceManifestSHA256 returns the identity of an ordered source closure.
func SourceManifestSHA256(roots []SourceManifestRoot) (string, error) {
	data, err := json.Marshal(roots)
	if err != nil {
		return "", fmt.Errorf("marshal source root manifest: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
