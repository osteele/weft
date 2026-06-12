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
//
// Two forms are supported:
//   - Job-output edge: Path + Version (producer job ID). AssetName is empty.
//     Resolved against the producer's artifacts in R2 or on disk.
//   - Named asset:     AssetName set. Path and Version are zero/empty;
//     the staging path comes from the named_assets row.
type ArtifactSpec struct {
	Path      string // relative path, e.g. "output/model.pt"
	Version   int64  // producer job ID, 0 means "use own job ID" (or named-asset form)
	AssetName string // non-empty for named-asset form (asset:NAME)
}

// IsAsset reports whether this spec references a named asset rather than a
// producer-job artifact.
func (s ArtifactSpec) IsAsset() bool { return s.AssetName != "" }

// ParseProducesSpec parses a --produces spec: "path" or "path:version".
func ParseProducesSpec(spec string) ArtifactSpec {
	path, version, ok := splitPathVersion(spec)
	if !ok {
		return ArtifactSpec{Path: spec}
	}
	return ArtifactSpec{Path: path, Version: version}
}

// ParseNeedsSpec parses a --needs spec. Accepts two forms:
//   - "path:version" — producer-job artifact (version is the producer job ID).
//   - "asset:NAME"   — named asset published via `weft data publish`.
func ParseNeedsSpec(spec string) (ArtifactSpec, error) {
	if name, ok := parseAssetSpec(spec); ok {
		if name == "" {
			return ArtifactSpec{}, fmt.Errorf("needs spec %q: asset name is empty", spec)
		}
		return ArtifactSpec{AssetName: name}, nil
	}
	path, version, ok := splitPathVersion(spec)
	if !ok {
		return ArtifactSpec{}, fmt.Errorf("needs spec %q must include :version suffix or use asset:NAME form", spec)
	}
	if version <= 0 {
		return ArtifactSpec{}, fmt.Errorf("needs spec %q has invalid version %d: must be positive", spec, version)
	}
	return ArtifactSpec{Path: path, Version: version}, nil
}

// parseAssetSpec returns (name, true) if spec matches "asset:NAME".
func parseAssetSpec(spec string) (string, bool) {
	const prefix = "asset:"
	if !strings.HasPrefix(spec, prefix) {
		return "", false
	}
	return spec[len(prefix):], true
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

// NamedAssetSatisfiedFile returns the satisfied marker file for a named asset
// staged from the asset store.
func NamedAssetSatisfiedFile(logDir string, name string) string {
	return artifacts.NamedAssetSatisfiedFile(logDir, name)
}

// RecordProducedArtifacts ensures --produces declarations are present in the
// artifact manifest, so they are uploaded/downloadable even when outside
// convention-based output directories.
func RecordProducedArtifacts(jobID int64, produces []string) error {
	return RecordDeclaredArtifacts(jobID, produces, nil)
}

// RecordDeclaredArtifacts ensures declared filesystem artifacts are present in
// the manifest consumed by cloud output uploaders.
func RecordDeclaredArtifacts(jobID int64, produces, outputs []string) error {
	if len(produces) == 0 && len(outputs) == 0 {
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
	for _, raw := range outputs {
		path, ok := FilesystemOutputRefPath(raw)
		if !ok || seen[path] {
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
