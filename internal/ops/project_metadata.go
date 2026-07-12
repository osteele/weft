package ops

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/workdir"
)

type dbExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// RefreshProjectDerivedMetadata updates a job's stored inputs and output dirs
// from the current project config and source scans, while preserving any
// existing explicit input declarations already recorded on the job.
func RefreshProjectDerivedMetadata(database dbExecer, job *db.Job) error {
	jobID, command := job.ID, job.Command
	localDir := workdir.ResolveLocal(job.WorkingDir)

	inputs := mergeStringSlices(config.ProjectInputs(localDir), explicitJobInputs(job))
	if meta, err := dataloc.ScanScriptMeta(localDir, command); err == nil && meta != nil {
		inputs = mergeStringSlices(inputs, meta.Inputs)
	}
	detected := mergeStringSlices(
		dataloc.ScanPythonHFRefsForCommand(localDir, command),
		dataloc.ScanCommandHFRefs(command),
	)
	bestEffortInputs := dataloc.FilterAutoDetectedInputs(detected, inputs)
	inputs = mergeStringSlices(inputs, bestEffortInputs)
	// Re-correct hf:-misprefixed datasets so a restart/requeue does not
	// reintroduce the raw hf: form from PEP-723 metadata.
	inputs, _ = dataloc.NormalizeMisprefixedHFDatasets(inputs)
	if err := db.SetJobInputs(database, jobID, inputs); err != nil {
		return fmt.Errorf("refresh job inputs: %w", err)
	}
	if err := setJobBestEffortInputs(database, job, bestEffortInputs); err != nil {
		return fmt.Errorf("refresh best-effort inputs: %w", err)
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

func explicitJobInputs(job *db.Job) []string {
	if len(job.Inputs) == 0 || len(job.BestEffortInputs) == 0 {
		return job.Inputs
	}
	bestEffort := make(map[string]struct{}, len(job.BestEffortInputs))
	for _, input := range job.BestEffortInputs {
		bestEffort[input] = struct{}{}
	}
	out := make([]string, 0, len(job.Inputs))
	for _, input := range job.Inputs {
		if _, ok := bestEffort[input]; ok {
			continue
		}
		out = append(out, input)
	}
	return out
}

func setJobBestEffortInputs(database dbExecer, job *db.Job, inputs []string) error {
	meta := job.Metadata
	if meta == nil {
		if len(inputs) == 0 {
			return nil
		}
		meta = &db.JobMetadata{}
	} else {
		copied := *meta
		meta = &copied
	}
	meta.BestEffortInputs = append([]string(nil), inputs...)
	return db.SetJobMetadata(database, job.ID, meta)
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
