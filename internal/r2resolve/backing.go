package r2resolve

import (
	"path"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/config"
)

// ConventionOutputRelPath returns the canonical workdir-relative path when a
// declared artifact can use the convention-output object as its durable R2
// backing. Custom artifact roots and absolute or escaping artifact paths keep
// their artifact-files backing because they do not have an unambiguous mapping
// into the job's working directory.
func ConventionOutputRelPath(manifest artifacts.Manifest, artifactPath string, outputDirs []string) (string, bool) {
	if strings.TrimSpace(manifest.ArtifactRoot) != "" {
		return "", false
	}
	artifactPath = strings.TrimSpace(artifactPath)
	if artifactPath == "" || strings.Contains(artifactPath, `\`) || strings.HasPrefix(artifactPath, "~") || filepath.IsAbs(artifactPath) || path.IsAbs(artifactPath) {
		return "", false
	}
	rel := path.Clean(artifactPath)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	for _, outputDir := range config.EffectiveOutputDirs(outputDirs) {
		outputDir = strings.TrimSpace(outputDir)
		if strings.Contains(outputDir, `\`) {
			continue
		}
		dir := path.Clean(outputDir)
		if dir == "." || dir == ".." || strings.HasPrefix(dir, "../") || path.IsAbs(dir) {
			continue
		}
		if rel == dir || strings.HasPrefix(rel, dir+"/") {
			return rel, true
		}
	}
	return "", false
}
