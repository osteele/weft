package artifacts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// RemoteArtifactsDir is the directory for artifact manifests on remote hosts.
const RemoteArtifactsDir = "~/.cache/weft/artifacts"

// RemoteManifestPath returns the remote manifest path for a job.
func RemoteManifestPath(jobID int64) string {
	return fmt.Sprintf("%s/%d.json", RemoteArtifactsDir, jobID)
}

// LocalArtifactsDir returns the local artifact store root.
func LocalArtifactsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "weft", "artifacts"), nil
}

// LocalJobDir returns the local artifact directory for a job.
func LocalJobDir(jobID int64) (string, error) {
	root, err := LocalArtifactsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, fmt.Sprintf("%d", jobID)), nil
}

// JobEnvVars returns environment variables injected into job shells.
// Both WEFT_* and legacy RJ_* prefixes are set for backward compatibility.
func JobEnvVars(jobID int64) []string {
	manifest := RemoteManifestPath(jobID)
	return []string{
		fmt.Sprintf("WEFT_JOB_ID=%d", jobID),
		fmt.Sprintf("WEFT_ARTIFACT_MANIFEST=%s", manifest),
		"WEFT_ARTIFACT_ROOT=.",
		fmt.Sprintf("RJ_JOB_ID=%d", jobID),
		fmt.Sprintf("RJ_ARTIFACT_MANIFEST=%s", manifest),
		"RJ_ARTIFACT_ROOT=.",
	}
}

// Manifest describes the artifacts produced by a job.
type Manifest struct {
	JobID        int64          `json:"job_id"`
	ArtifactRoot string         `json:"artifact_root,omitempty"`
	Artifacts    []ArtifactSpec `json:"artifacts"`
}

// ArtifactSpec identifies a single artifact path.
type ArtifactSpec struct {
	Name string `json:"name,omitempty"`
	Path string `json:"path"`
}

// ErrManifestMissing is returned when no manifest is found.
var ErrManifestMissing = errors.New("artifact manifest not found")

// ParseManifest parses manifest content. It accepts JSON format or plain-text
// (one file path per line) for scripts that append paths to $WEFT_ARTIFACT_MANIFEST.
func ParseManifest(content string, fallbackJobID int64) (Manifest, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return Manifest{}, ErrManifestMissing
	}

	var manifest Manifest
	if trimmed[0] == '{' {
		if err := json.Unmarshal([]byte(content), &manifest); err != nil {
			return Manifest{}, err
		}
		if manifest.JobID == 0 {
			manifest.JobID = fallbackJobID
		}
		return manifest, nil
	}

	// Plain-text format: one path per line
	manifest = Manifest{JobID: fallbackJobID}
	for _, line := range strings.Split(trimmed, "\n") {
		p := strings.TrimSpace(line)
		if p == "" {
			continue
		}
		manifest.Artifacts = append(manifest.Artifacts, ArtifactSpec{Path: p})
	}
	if len(manifest.Artifacts) == 0 {
		return Manifest{}, ErrManifestMissing
	}
	return manifest, nil
}

// ReadManifestFile reads a manifest from disk, returning an empty manifest if missing.
func ReadManifestFile(path string, jobID int64) (Manifest, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Manifest{JobID: jobID}, nil
		}
		return Manifest{}, err
	}
	return ParseManifest(string(content), jobID)
}

// WriteManifestFile writes a manifest to disk.
func WriteManifestFile(path string, manifest Manifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// ResolveArtifactRoot determines the root used to resolve relative artifact paths.
func ResolveArtifactRoot(manifest Manifest, workingDir string) string {
	if manifest.ArtifactRoot != "" {
		return manifest.ArtifactRoot
	}
	if workingDir != "" {
		return workingDir
	}
	return "~"
}

// ResolveRemotePath joins root and artifact path when needed.
func ResolveRemotePath(root, artifactPath string) string {
	if artifactPath == "" {
		return ""
	}
	if strings.HasPrefix(artifactPath, "/") || strings.HasPrefix(artifactPath, "~") {
		return artifactPath
	}
	if root == "" {
		return artifactPath
	}
	return path.Join(root, artifactPath)
}

// LocalRelativePath returns a safe relative path for storing an artifact.
func LocalRelativePath(input string) string {
	cleaned := path.Clean(strings.TrimSpace(input))
	if cleaned == "." || cleaned == "" {
		return "artifact"
	}
	cleaned = strings.TrimPrefix(cleaned, "./")
	if strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "../") || cleaned == ".." || strings.Contains(cleaned, "../") {
		cleaned = path.Base(cleaned)
	}
	return filepath.FromSlash(cleaned)
}

// LocalStoredPath returns the stored path relative to the local artifacts root.
func LocalStoredPath(jobID int64, artifactPath string) string {
	return filepath.Join(fmt.Sprintf("%d", jobID), LocalRelativePath(artifactPath))
}

// LocalPathFromStored returns the absolute local path for a stored artifact.
func LocalPathFromStored(storedPath string) (string, error) {
	root, err := LocalArtifactsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, storedPath), nil
}

// MergeEnvVars appends artifact env vars, preserving any user overrides.
func MergeEnvVars(envVars []string, jobID int64) []string {
	existing := map[string]bool{}
	for _, ev := range envVars {
		if key, _, ok := strings.Cut(ev, "="); ok {
			existing[key] = true
		}
	}
	for _, ev := range JobEnvVars(jobID) {
		if key, _, ok := strings.Cut(ev, "="); ok {
			if existing[key] {
				continue
			}
		}
		envVars = append(envVars, ev)
	}
	return envVars
}
