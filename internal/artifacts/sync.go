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

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ssh"
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
		storedPath := LocalStoredPath(job.ID, spec.Path)
		localPath := filepath.Join(localRoot, storedPath)
		if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
			return result, err
		}
		if err := ssh.CopyFromWithRetry(remotePath, job.Host, localPath); err != nil {
			return result, err
		}
		size, sha, err := hashFile(localPath)
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
