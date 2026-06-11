package artifacts

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ssh"
)

// SyncResult captures artifact sync results.
type SyncResult struct {
	Added   int
	Skipped int
}

// FetchManifest loads the artifact manifest from the remote host.
func FetchManifest(host string, jobID int64, timeout time.Duration) (Manifest, error) {
	manifestPath := RemoteManifestPath(jobID)
	cmd := fmt.Sprintf("cat %s 2>/dev/null", manifestPath)
	stdout, stderr, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		if strings.TrimSpace(stdout) == "" {
			lower := strings.ToLower(stderr)
			if strings.Contains(lower, "no such file") || strings.Contains(lower, "not found") || strings.TrimSpace(stderr) == "" {
				return Manifest{}, ErrManifestMissing
			}
		}
		return Manifest{}, err
	}
	return ParseManifest(stdout, jobID)
}

// SyncJob fetches artifacts from a remote host into the local store and DB.
func SyncJob(database *sql.DB, job *db.Job, timeout time.Duration) (SyncResult, error) {
	manifest, err := FetchManifest(job.Host, job.ID, timeout)
	if err != nil {
		return SyncResult{}, err
	}
	root := ResolveArtifactRoot(manifest, job.WorkingDir)

	localRoot, err := LocalArtifactsDir()
	if err != nil {
		return SyncResult{}, err
	}

	result := SyncResult{}
	for _, spec := range manifest.Artifacts {
		if strings.TrimSpace(spec.Path) == "" {
			result.Skipped++
			continue
		}
		remotePath := ResolveRemotePath(root, spec.Path)
		storedPath, size, sha, err := copyRemoteArtifact(job, spec, remotePath, localRoot, ssh.CopyFromWithRetry)
		if err != nil {
			return result, err
		}
		if err := db.UpsertArtifact(database, db.Artifact{
			JobID:      job.ID,
			Name:       spec.Name,
			Path:       spec.Path,
			StoredPath: storedPath,
			SizeBytes:  size,
			SHA256:     sha,
		}); err != nil {
			return result, err
		}
		result.Added++
	}
	return result, nil
}

// SyncOutstandingJob fetches artifacts that are not already cached locally.
func SyncOutstandingJob(database *sql.DB, job *db.Job, timeout time.Duration) (SyncResult, error) {
	manifest, err := FetchManifest(job.Host, job.ID, timeout)
	if err != nil {
		return SyncResult{}, err
	}
	root := ResolveArtifactRoot(manifest, job.WorkingDir)

	localRoot, err := LocalArtifactsDir()
	if err != nil {
		return SyncResult{}, err
	}

	existing, err := db.ListArtifactsByJob(database, job.ID)
	if err != nil {
		return SyncResult{}, err
	}
	existingByPath := make(map[string]db.Artifact, len(existing))
	for _, art := range existing {
		existingByPath[art.Path] = art
	}

	result := SyncResult{}
	for _, spec := range manifest.Artifacts {
		if strings.TrimSpace(spec.Path) == "" {
			result.Skipped++
			continue
		}

		if art, ok := existingByPath[spec.Path]; ok {
			if art.StoredPath != "" {
				localPath := filepath.Join(localRoot, art.StoredPath)
				if _, err := os.Stat(localPath); err == nil {
					result.Skipped++
					continue
				}
			}
		}

		remotePath := ResolveRemotePath(root, spec.Path)
		storedPath, size, sha, err := copyRemoteArtifact(job, spec, remotePath, localRoot, ssh.CopyFromWithRetry)
		if err != nil {
			return result, err
		}
		if err := db.UpsertArtifact(database, db.Artifact{
			JobID:      job.ID,
			Name:       spec.Name,
			Path:       spec.Path,
			StoredPath: storedPath,
			SizeBytes:  size,
			SHA256:     sha,
		}); err != nil {
			return result, err
		}
		result.Added++
	}
	return result, nil
}

func copyRemoteArtifact(job *db.Job, spec ArtifactSpec, remotePath, localRoot string, copyFrom func(remotePath, host, localPath string) error) (storedPath string, size int64, sha string, err error) {
	storedPath = LocalStoredPath(job.ID, spec.Path)
	localPath := filepath.Join(localRoot, storedPath)
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return "", 0, "", err
	}
	if err := copyFrom(remotePath, job.Host, localPath); err != nil {
		return "", 0, "", fmt.Errorf("copy artifact %q from %s:%s to %s: %w", spec.Path, job.Host, remotePath, localPath, err)
	}
	size, sha, err = hashFile(localPath)
	if err != nil {
		return "", 0, "", err
	}
	return storedPath, size, sha, nil
}

// StoreLocalArtifact copies a local file into the artifact cache and records it
// in the artifact database.
func StoreLocalArtifact(database *sql.DB, jobID int64, relPath, sourcePath string) error {
	if strings.TrimSpace(relPath) == "" {
		return nil
	}
	localRoot, err := LocalArtifactsDir()
	if err != nil {
		return err
	}

	storedPath := LocalStoredPath(jobID, relPath)
	localPath := filepath.Join(localRoot, storedPath)
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	if err := copyLocalFile(sourcePath, localPath); err != nil {
		return err
	}
	size, sha, err := hashFile(localPath)
	if err != nil {
		return err
	}
	return db.UpsertArtifact(database, db.Artifact{
		JobID:      jobID,
		Path:       relPath,
		StoredPath: storedPath,
		SizeBytes:  size,
		SHA256:     sha,
	})
}

func copyLocalFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func hashFile(path string) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()

	h := sha256.New()
	size, err := io.Copy(h, file)
	if err != nil {
		return 0, "", err
	}
	return size, fmt.Sprintf("%x", h.Sum(nil)), nil
}
