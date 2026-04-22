package runner

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/artifacts"
)

// ArtifactSpec represents a parsed artifact dependency specification.
type ArtifactSpec struct {
	Path    string // relative path, e.g. "output/model.pt"
	Version int64  // producer job ID, 0 means "use own job ID"
}

// ParseProducesSpec parses a --produces spec: "path" or "path:version".
func ParseProducesSpec(spec string) ArtifactSpec {
	path, version, ok := splitPathVersion(spec)
	if !ok {
		return ArtifactSpec{Path: spec}
	}
	return ArtifactSpec{Path: path, Version: version}
}

// ParseNeedsSpec parses a --needs spec: "path:version" (version is required).
func ParseNeedsSpec(spec string) (ArtifactSpec, error) {
	path, version, ok := splitPathVersion(spec)
	if !ok {
		return ArtifactSpec{}, fmt.Errorf("needs spec %q must include :version suffix", spec)
	}
	if version <= 0 {
		return ArtifactSpec{}, fmt.Errorf("needs spec %q has invalid version %d: must be positive", spec, version)
	}
	return ArtifactSpec{Path: path, Version: version}, nil
}

// splitPathVersion splits "path:int64" on the last colon and parses the version.
// Returns (path, version, true) on success, or ("", 0, false) if no valid version suffix.
func splitPathVersion(spec string) (string, int64, bool) {
	idx := strings.LastIndex(spec, ":")
	if idx < 0 {
		return "", 0, false
	}
	version, err := strconv.ParseInt(spec[idx+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return spec[:idx], version, true
}

// ArtifactSatisfiedFile returns the path to the satisfied marker file for an artifact.
// The path component is URL-encoded to avoid slashes in filenames.
// Note: these files live in the same logDir as job files (see NewJobPaths),
// but use a distinct naming scheme: artifact-{version}-{encodedPath}.satisfied
func ArtifactSatisfiedFile(logDir string, path string, version int64) string {
	encoded := url.PathEscape(path)
	return filepath.Join(logDir, fmt.Sprintf("artifact-%d-%s.satisfied", version, encoded))
}

// RecordProducedArtifacts ensures --produces declarations are present in the
// artifact manifest, so they are uploaded/downloadable even when outside
// convention-based output directories.
func RecordProducedArtifacts(jobID int64, produces []string) error {
	if len(produces) == 0 {
		return nil
	}
	manifestPath := ExpandTilde(artifacts.RemoteManifestPath(jobID))
	manifest, err := artifacts.ReadManifestFile(manifestPath, jobID)
	if err != nil {
		return err
	}
	if manifest.JobID == 0 {
		manifest.JobID = jobID
	}

	seen := make(map[string]bool, len(manifest.Artifacts))
	for _, spec := range manifest.Artifacts {
		path := strings.TrimSpace(spec.Path)
		if path != "" {
			seen[path] = true
		}
	}

	changed := false
	for _, raw := range produces {
		parsed := ParseProducesSpec(raw)
		path := strings.TrimSpace(parsed.Path)
		if path == "" || seen[path] {
			continue
		}
		manifest.Artifacts = append(manifest.Artifacts, artifacts.ArtifactSpec{Path: path})
		seen[path] = true
		changed = true
	}
	if !changed {
		return nil
	}
	return artifacts.WriteManifestFile(manifestPath, manifest)
}
