package ops

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/workdir"
)

// RefreshProjectDerivedMetadata updates a job's stored inputs and output dirs
// from the current project config and source scans, while preserving any
// existing explicit input declarations already recorded on the job.
func RefreshProjectDerivedMetadata(database *sql.DB, jobID int64, workingDir, command string, existingInputs []string) error {
	localDir := workdir.ResolveLocal(workingDir)

	inputs := mergeStringSlices(config.ProjectInputs(localDir), existingInputs)
	if detected := dataloc.ScanPythonHFRefs(localDir); len(detected) > 0 {
		inputs = mergeStringSlices(inputs, detected)
	}
	if detected := dataloc.ScanCommandHFRefs(command); len(detected) > 0 {
		inputs = mergeStringSlices(inputs, detected)
	}
	if err := db.SetJobInputs(database, jobID, inputs); err != nil {
		return fmt.Errorf("refresh job inputs: %w", err)
	}

	if err := db.SetJobOutputDirs(database, jobID, config.ProjectOutputDirs(localDir)); err != nil {
		return fmt.Errorf("refresh job output dirs: %w", err)
	}

	return nil
}

func mergeStringSlices(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, items := range [][]string{a, b} {
		for _, item := range items {
			if _, ok := seen[item]; ok {
				continue
			}
			seen[item] = struct{}{}
			out = append(out, item)
		}
	}
	return out
}
