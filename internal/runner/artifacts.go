package runner

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/artifactspec"
)

type ArtifactSpec = artifactspec.ArtifactSpec

// ParseProducesSpec parses a --produces spec: "path" or "path:version".
func ParseProducesSpec(spec string) ArtifactSpec {
	return artifactspec.ParseProducesSpec(spec)
}

// ParseNeedsSpec parses a --needs spec. Accepts two forms:
//   - "path:version" — producer-job artifact (version is the producer job ID).
//   - "asset:NAME"   — named asset published via `weft data publish`.
func ParseNeedsSpec(spec string) (ArtifactSpec, error) {
	return artifactspec.ParseNeedsSpec(spec)
}

// ValidateNeedsSpecs rejects malformed --needs values before they are persisted.
func ValidateNeedsSpecs(specs []string) error {
	return artifactspec.ValidateNeedsSpecs(specs)
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
