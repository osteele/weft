package artifacts

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/osteele/weft/internal/dataplane"
)

// PayloadFetcher materializes one content-addressed object at destination.
type PayloadFetcher func(r2Key, destination string) error

// StagePayloads materializes and verifies one job's payload directory. The
// staged copies are owner-readable/writable; their permissions are privacy
// controls, not an immutability mechanism for the job process.
func StagePayloads(jobID int64, payloads []dataplane.JobPayload, fetch PayloadFetcher) (string, error) {
	if len(payloads) == 0 {
		return "", nil
	}
	dir := expandTildeWithEnv(PayloadStagingDir(jobID), nil)
	if dir == "" {
		return "", fmt.Errorf("resolve payload staging directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	seen := make(map[string]struct{}, len(payloads))
	for _, payload := range payloads {
		if err := ValidatePayloadName(payload.Name); err != nil {
			return "", err
		}
		if _, ok := seen[payload.Name]; ok {
			return "", fmt.Errorf("duplicate payload %q", payload.Name)
		}
		seen[payload.Name] = struct{}{}
		target := filepath.Join(dir, payload.Name)
		if ok, err := VerifyPayloadFile(target, payload.SizeBytes, payload.SHA256); err != nil {
			return "", err
		} else if ok {
			if err := os.Chmod(target, 0o600); err != nil {
				return "", err
			}
			continue
		}
		tmp, err := os.CreateTemp(dir, ".payload-*")
		if err != nil {
			return "", err
		}
		tmpPath := tmp.Name()
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmpPath)
			return "", err
		}
		if err := fetch(payload.R2Key, tmpPath); err != nil {
			_ = os.Remove(tmpPath)
			return "", fmt.Errorf("fetch payload %q: %w", payload.Name, err)
		}
		ok, err := VerifyPayloadFile(tmpPath, payload.SizeBytes, payload.SHA256)
		if err != nil {
			_ = os.Remove(tmpPath)
			return "", err
		}
		if !ok {
			_ = os.Remove(tmpPath)
			return "", fmt.Errorf("payload %q failed size or SHA-256 verification", payload.Name)
		}
		if err := os.Chmod(tmpPath, 0o600); err != nil {
			_ = os.Remove(tmpPath)
			return "", err
		}
		if err := os.Rename(tmpPath, target); err != nil {
			_ = os.Remove(tmpPath)
			return "", err
		}
	}
	return dir, nil
}

// VerifyPayloadFile reports whether filename is a regular file with the
// promised size and SHA-256 digest.
func VerifyPayloadFile(filename string, size int64, digest string) (bool, error) {
	file, err := os.Open(filename)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return false, nil
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false, err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)) == digest, nil
}
