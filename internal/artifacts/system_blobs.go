package artifacts

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CaptureSystemBlob atomically stores immutable system-owned bytes in the
// artifact data root. The namespace is an internal, portable path component;
// callers retain the returned relative path in their typed database record.
func CaptureSystemBlob(namespace string, data []byte) (storedPath string, size int64, digest string, err error) {
	if err := ValidatePayloadName(namespace); err != nil {
		return "", 0, "", fmt.Errorf("invalid system-blob namespace: %w", err)
	}
	digest = fmt.Sprintf("%x", sha256.Sum256(data))
	size = int64(len(data))
	storedPath = filepath.Join("system", namespace, "sha256", digest)
	dest, err := LocalPathFromStored(storedPath)
	if err != nil {
		return "", 0, "", err
	}
	if ok, verifyErr := VerifyPayloadFile(dest, size, digest); verifyErr != nil {
		return "", 0, "", verifyErr
	} else if ok {
		return storedPath, size, digest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return "", 0, "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".capture-*")
	if err != nil {
		return "", 0, "", err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", 0, "", err
	}
	if _, err := tmp.Write(data); err != nil {
		return "", 0, "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, "", err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, "", err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return "", 0, "", err
	}
	return storedPath, size, digest, nil
}

// ReadSystemBlob reads and verifies a relative path previously returned by
// CaptureSystemBlob. Database corruption must not turn a stored path into an
// arbitrary filesystem read.
func ReadSystemBlob(storedPath string, size int64, digest string) ([]byte, error) {
	clean := filepath.Clean(strings.TrimSpace(storedPath))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("invalid stored artifact path %q", storedPath)
	}
	path, err := LocalPathFromStored(clean)
	if err != nil {
		return nil, err
	}
	ok, err := VerifyPayloadFile(path, size, digest)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("stored artifact %s failed size or digest verification", clean)
	}
	return os.ReadFile(path)
}

func VerifyBlobBytes(data []byte, size int64, digest string) error {
	if int64(len(data)) != size {
		return fmt.Errorf("object size %d does not match recorded size %d", len(data), size)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(data))
	if got != digest {
		return fmt.Errorf("object SHA-256 %s does not match recorded digest %s", got, digest)
	}
	return nil
}
