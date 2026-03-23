package sync

import (
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/workdir"
)

// CollectExtraPaths gathers file paths to sync from input flags and .weft.toml.
// It classifies inputs into asset refs (ignored here) and file paths,
// then merges with extra_paths from the project config if found.
// Relative paths (e.g., from local: inputs) are resolved against localDir
// and converted to tilde-relative form for portable local/remote syncing.
func CollectExtraPaths(inputs []string, localDir string) []string {
	_, filePaths := dataloc.ClassifyInputs(inputs)
	filePaths = append(filePaths, config.ProjectExtraPaths(localDir)...)
	for i, p := range filePaths {
		if !filepath.IsAbs(p) && !strings.HasPrefix(p, "~/") && p != "" {
			abs := filepath.Join(localDir, p)
			filePaths[i] = workdir.ToTildeRelative(abs)
		}
	}
	return filePaths
}
