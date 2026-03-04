package sync

import (
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
)

// CollectExtraPaths gathers file paths to sync from input flags and .weft.yaml.
// It classifies inputs into asset refs (ignored here) and file paths,
// then merges with extra_paths from the project config if found.
func CollectExtraPaths(inputs []string, localDir string) []string {
	_, filePaths := dataloc.ClassifyInputs(inputs)
	filePaths = append(filePaths, config.ProjectExtraPaths(localDir)...)
	return filePaths
}
