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

type ProjectDerivedMetadata struct {
	Inputs           []string
	BestEffortInputs []string
	OutputDirs       []string
	MaxComputeCap    string
}

// ComputeProjectDerivedMetadata reads current project and script declarations
// without mutating the job or database.
func ComputeProjectDerivedMetadata(job *db.Job) ProjectDerivedMetadata {
	command := job.Command
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

	maxCap := ""
	if job.RequestsGPU() {
		maxCap = ResolveProjectMaxComputeCap(localDir, command)
	}
	return ProjectDerivedMetadata{
		Inputs:           inputs,
		BestEffortInputs: bestEffortInputs,
		OutputDirs:       config.ProjectOutputDirs(localDir),
		MaxComputeCap:    maxCap,
	}
}

// ApplyProjectDerivedMetadata updates only the in-memory job.
func ApplyProjectDerivedMetadata(job *db.Job, derived ProjectDerivedMetadata) {
	job.Inputs = append([]string(nil), derived.Inputs...)
	job.BestEffortInputs = append([]string(nil), derived.BestEffortInputs...)
	job.OutputDirs = append([]string(nil), derived.OutputDirs...)
	job.MaxComputeCap = derived.MaxComputeCap

	meta := job.Metadata
	if meta != nil {
		copied := *meta
		meta = &copied
	} else if len(derived.BestEffortInputs) > 0 {
		meta = &db.JobMetadata{}
	}
	if meta != nil {
		meta.BestEffortInputs = append([]string(nil), derived.BestEffortInputs...)
		job.Metadata = meta
	}
}

// PersistProjectDerivedMetadata writes fields already applied to job.
func PersistProjectDerivedMetadata(database dbExecer, job *db.Job) error {
	if err := db.SetJobInputs(database, job.ID, job.Inputs); err != nil {
		return fmt.Errorf("refresh job inputs: %w", err)
	}
	if err := db.SetJobMetadata(database, job.ID, job.Metadata); err != nil {
		return fmt.Errorf("refresh best-effort inputs: %w", err)
	}
	if err := db.SetJobOutputDirs(database, job.ID, job.OutputDirs); err != nil {
		return fmt.Errorf("refresh job output dirs: %w", err)
	}
	if err := db.SetJobMaxComputeCap(database, job.ID, job.MaxComputeCap); err != nil {
		return fmt.Errorf("refresh max_compute_cap: %w", err)
	}
	return nil
}

// RefreshProjectDerivedMetadata updates a job's stored inputs and output dirs
// from the current project config and source scans, while preserving any
// existing explicit input declarations already recorded on the job.
func RefreshProjectDerivedMetadata(database dbExecer, job *db.Job) error {
	derived := ComputeProjectDerivedMetadata(job)
	ApplyProjectDerivedMetadata(job, derived)
	return PersistProjectDerivedMetadata(database, job)
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

// ResolveProjectMaxComputeCap resolves the persisted cap encoding for project
// metadata refreshes. Keep this in ops to avoid importing placement here; the
// launch path still re-resolves empty caps as a second line of defense.
func ResolveProjectMaxComputeCap(localDir, command string) string {
	return dataloc.ResolveJobTorchMaxComputeCapForPersistence(localDir, command)
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
