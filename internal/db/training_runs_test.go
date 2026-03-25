package db

import (
	"database/sql"
	"encoding/json"
	"testing"
)

func TestJobRunSnapshotsFreezeSpecAcrossReruns(t *testing.T) {
	t.Skip("job_runs archival removed; job_attempts tracks history instead")
}

func TestListTrainingJobRunsUsesPerRunSnapshotsAndTimeseries(t *testing.T) {
	t.Skip("job_runs archival removed; job_attempts tracks history instead")
}

func TestSetJobPlacementMetaDoesNotCreateRunBeforeStart(t *testing.T) {
	t.Skip("job_runs archival removed")
}

func TestSetJobPlacementReasonsDoesNotCreateRunBeforeStart(t *testing.T) {
	t.Skip("job_runs archival removed")
}

func TestOpenBackfillsLegacyTerminalJobsIntoRuns(t *testing.T) {
	t.Skip("job_runs archival removed")
}

type seededRunSpec struct {
	command      string
	workingDir   string
	description  string
	gpu          string
	gpuClass     string
	cpuAllotment *int
	gpuMemGB     *int
	depSpec      string
	inputs       []string
	outputs      []string
	outputDirs   []string
	produces     []string
	needs        []string
	project      string
}

func seedJobRunSpec(database *sql.DB, jobID int64, spec seededRunSpec) error {
	makeJSON := func(values []string) any {
		if len(values) == 0 {
			return nil
		}
		data, _ := json.Marshal(values)
		return string(data)
	}
	_, err := database.Exec(
		`UPDATE jobs SET command = COALESCE(NULLIF(?, ''), command),
		                working_dir = COALESCE(NULLIF(?, ''), working_dir),
		                description = COALESCE(NULLIF(?, ''), description),
		                gpu = COALESCE(NULLIF(?, ''), gpu),
		                gpu_class = COALESCE(NULLIF(?, ''), gpu_class),
		                cpu_allotment = COALESCE(?, cpu_allotment),
		                gpu_mem_gb = COALESCE(?, gpu_mem_gb),
		                dep_spec = COALESCE(NULLIF(?, ''), dep_spec),
		                inputs = COALESCE(?, inputs),
		                outputs = COALESCE(?, outputs),
		                output_dirs = COALESCE(?, output_dirs),
		                produces = COALESCE(?, produces),
		                needs = COALESCE(?, needs),
		                project = COALESCE(NULLIF(?, ''), project)
		  WHERE id = ?`,
		spec.command, spec.workingDir, spec.description, spec.gpu, spec.gpuClass,
		spec.cpuAllotment, spec.gpuMemGB, spec.depSpec,
		makeJSON(spec.inputs), makeJSON(spec.outputs), makeJSON(spec.outputDirs),
		makeJSON(spec.produces), makeJSON(spec.needs), spec.project, jobID,
	)
	return err
}

func intPtr(v int) *int { return &v }

func int64Ptr(v int64) *int64 { return &v }

func float64Ptr(v float64) *float64 { return &v }
