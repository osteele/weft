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
func RefreshProjectDerivedMetadata(database *sql.DB, job *db.Job) error {
	jobID, command := job.ID, job.Command
	localDir := workdir.ResolveLocal(job.WorkingDir)

	inputs := mergeStringSlices(config.ProjectInputs(localDir), job.Inputs)
	if detected := dataloc.ScanPythonHFRefsForCommand(localDir, command); len(detected) > 0 {
		inputs = mergeStringSlices(inputs, detected)
	}
	if detected := dataloc.ScanCommandHFRefs(command); len(detected) > 0 {
		inputs = mergeStringSlices(inputs, detected)
	}
	if meta, err := dataloc.ScanScriptMeta(localDir, command); err == nil && meta != nil {
		inputs = mergeStringSlices(inputs, meta.Inputs)
	}
	// Re-correct hf:-misprefixed datasets so a restart/requeue does not
	// reintroduce the raw hf: form from PEP-723 metadata.
	inputs, _ = dataloc.NormalizeMisprefixedHFDatasets(inputs)
	if err := db.SetJobInputs(database, jobID, inputs); err != nil {
		return fmt.Errorf("refresh job inputs: %w", err)
	}

	if err := db.SetJobOutputDirs(database, jobID, config.ProjectOutputDirs(localDir)); err != nil {
		return fmt.Errorf("refresh job output dirs: %w", err)
	}

	// The torch-derived arch cap applies only to GPU jobs; refreshing a
	// CPU-only job clears any stale inert cap rather than recomputing one.
	maxCap := ""
	if job.RequestsGPU() {
		maxCap = ResolveProjectMaxComputeCap(localDir, command)
	}
	if err := db.SetJobMaxComputeCap(database, jobID, maxCap); err != nil {
		return fmt.Errorf("refresh max_compute_cap: %w", err)
	}

	return nil
}

// ResolveProjectMaxComputeCap resolves the persisted cap encoding for project
// metadata refreshes. Keep this in ops to avoid importing placement here; the
// launch path still re-resolves empty caps as a second line of defense.
func ResolveProjectMaxComputeCap(localDir, command string) string {
	archMax := ""
	if meta, err := dataloc.ScanScriptMeta(localDir, command); err == nil && meta != nil {
		archMax = meta.GPUArchMax
	}
	return dataloc.ResolveTorchMaxComputeCapForPersistence(archMax, localDir)
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
