package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var payloadNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidatePayloadName requires one portable, non-special path component.
func ValidatePayloadName(name string) error {
	if len(name) > 128 || !payloadNamePattern.MatchString(name) || name == "." || name == ".." {
		return fmt.Errorf("payload name %q must start with a letter or digit and contain only letters, digits, '.', '_', or '-'", name)
	}
	return nil
}

// CapturePayload copies a regular file into the content-addressed local
// artifact store while hashing the copied bytes. The returned path is relative
// to LocalArtifactsDir and no longer depends on the source path.
func CapturePayload(sourcePath string) (storedPath string, size int64, digest string, err error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", 0, "", err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return "", 0, "", err
	}
	if !info.Mode().IsRegular() {
		return "", 0, "", fmt.Errorf("not a regular file")
	}

	root, err := LocalArtifactsDir()
	if err != nil {
		return "", 0, "", err
	}
	payloadRoot := filepath.Join(root, "payloads")
	if err := os.MkdirAll(payloadRoot, 0o700); err != nil {
		return "", 0, "", err
	}
	tmp, err := os.CreateTemp(payloadRoot, ".capture-*")
	if err != nil {
		return "", 0, "", err
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", 0, "", err
	}
	hash := sha256.New()
	size, err = io.Copy(io.MultiWriter(tmp, hash), source)
	if err != nil {
		return "", 0, "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, "", err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, "", err
	}
	digest = hex.EncodeToString(hash.Sum(nil))
	storedPath = filepath.Join("payloads", digest)
	dest := filepath.Join(root, storedPath)
	if matches, err := VerifyPayloadFile(dest, size, digest); err != nil {
		return "", 0, "", err
	} else if matches {
		return storedPath, size, digest, nil
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return "", 0, "", err
	}
	keep = true
	return storedPath, size, digest, nil
}

// PayloadStagingDir is the owner-private directory exposed as
// WEFT_PAYLOAD_DIR for a logical job.
func PayloadStagingDir(jobID int64) string {
	return filepath.Join("~/.cache/weft/payloads", fmt.Sprintf("%d", jobID))
}
