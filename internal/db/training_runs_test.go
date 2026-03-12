package db

import (
	"database/sql"
	"encoding/json"
	"os"
	"testing"
)

func TestJobRunSnapshotsFreezeSpecAcrossReruns(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "host1", "/tmp/project-old", "python old.py", "old desc")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := seedJobRunSpec(database, jobID, seededRunSpec{
		command:      "python old.py",
		workingDir:   "/tmp/project-old",
		description:  "old desc",
		gpu:          "0",
		gpuClass:     "A100",
		cpuAllotment: intPtr(70),
		gpuMemGB:     intPtr(40),
		depSpec:      "7+",
		inputs:       []string{"hf:old-model"},
		outputs:      []string{"artifact:old"},
		outputDirs:   []string{"output/"},
		produces:     []string{"output/model.bin"},
		needs:        []string{"input/data.json"},
		project:      "project-old",
	}); err != nil {
		t.Fatalf("seed old spec: %v", err)
	}
	if err := SetJobEnvVars(database, jobID, []string{"A=1"}); err != nil {
		t.Fatalf("SetJobEnvVars old: %v", err)
	}
	if err := SetJobTags(database, jobID, []string{"old", "train"}); err != nil {
		t.Fatalf("SetJobTags old: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning old: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID old: %v", err)
	}
	if err := RecordCompletionByID(database, jobID, 0, job.StartTime+5); err != nil {
		t.Fatalf("RecordCompletionByID old: %v", err)
	}
	if err := RequeueByID(database, jobID); err != nil {
		t.Fatalf("RequeueByID: %v", err)
	}

	if err := seedJobRunSpec(database, jobID, seededRunSpec{
		command:      "python new.py --flag",
		workingDir:   "/tmp/project-new",
		description:  "new desc",
		gpu:          "1",
		gpuClass:     "H100",
		cpuAllotment: intPtr(90),
		gpuMemGB:     intPtr(80),
		depSpec:      "9",
		inputs:       []string{"hf:new-model"},
		outputs:      []string{"artifact:new"},
		outputDirs:   []string{"results/"},
		produces:     []string{"results/model.bin"},
		needs:        []string{"input/new.json"},
		project:      "project-new",
	}); err != nil {
		t.Fatalf("seed new spec: %v", err)
	}
	if err := SetJobEnvVars(database, jobID, []string{"B=2"}); err != nil {
		t.Fatalf("SetJobEnvVars new: %v", err)
	}
	if err := SetJobTags(database, jobID, []string{"new", "serve"}); err != nil {
		t.Fatalf("SetJobTags new: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning new: %v", err)
	}

	runs, err := ListJobRuns(database, jobID)
	if err != nil {
		t.Fatalf("ListJobRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("run count = %d, want 2", len(runs))
	}

	oldRun := runs[0]
	if oldRun.Command != "python old.py" || oldRun.WorkingDir != "/tmp/project-old" || oldRun.Description != "old desc" {
		t.Fatalf("old run identity = (%q, %q, %q)", oldRun.Command, oldRun.WorkingDir, oldRun.Description)
	}
	if oldRun.GPU != "0" || oldRun.GPUClass != "A100" || oldRun.Project != "project-old" || oldRun.DepSpec != "7+" {
		t.Fatalf("old run spec = gpu %q class %q project %q dep %q", oldRun.GPU, oldRun.GPUClass, oldRun.Project, oldRun.DepSpec)
	}
	if oldRun.CPUAllotment == nil || *oldRun.CPUAllotment != 70 || oldRun.GPUMemGB == nil || *oldRun.GPUMemGB != 40 {
		t.Fatalf("old run cpu/gpu mem = %v %v", oldRun.CPUAllotment, oldRun.GPUMemGB)
	}
	if len(oldRun.EnvVars) != 1 || oldRun.EnvVars[0] != "A=1" {
		t.Fatalf("old run env = %v", oldRun.EnvVars)
	}
	if len(oldRun.Tags) != 2 || oldRun.Tags[0] != "old" || oldRun.Tags[1] != "train" {
		t.Fatalf("old run tags = %v", oldRun.Tags)
	}
	if len(oldRun.Inputs) != 1 || oldRun.Inputs[0] != "hf:old-model" || len(oldRun.OutputDirs) != 1 || oldRun.OutputDirs[0] != "output/" {
		t.Fatalf("old run inputs/output_dirs = %v %v", oldRun.Inputs, oldRun.OutputDirs)
	}

	newRun := runs[1]
	if newRun.Command != "python new.py --flag" || newRun.WorkingDir != "/tmp/project-new" || newRun.Description != "new desc" {
		t.Fatalf("new run identity = (%q, %q, %q)", newRun.Command, newRun.WorkingDir, newRun.Description)
	}
	if newRun.GPU != "1" || newRun.GPUClass != "H100" || newRun.Project != "project-new" || newRun.DepSpec != "9" {
		t.Fatalf("new run spec = gpu %q class %q project %q dep %q", newRun.GPU, newRun.GPUClass, newRun.Project, newRun.DepSpec)
	}
	if newRun.CPUAllotment == nil || *newRun.CPUAllotment != 90 || newRun.GPUMemGB == nil || *newRun.GPUMemGB != 80 {
		t.Fatalf("new run cpu/gpu mem = %v %v", newRun.CPUAllotment, newRun.GPUMemGB)
	}
	if len(newRun.EnvVars) != 1 || newRun.EnvVars[0] != "B=2" {
		t.Fatalf("new run env = %v", newRun.EnvVars)
	}
	if len(newRun.Tags) != 2 || newRun.Tags[0] != "new" || newRun.Tags[1] != "serve" {
		t.Fatalf("new run tags = %v", newRun.Tags)
	}
	if len(newRun.Inputs) != 1 || newRun.Inputs[0] != "hf:new-model" || len(newRun.OutputDirs) != 1 || newRun.OutputDirs[0] != "results/" {
		t.Fatalf("new run inputs/output_dirs = %v %v", newRun.Inputs, newRun.OutputDirs)
	}
}

func TestListTrainingJobRunsUsesPerRunSnapshotsAndTimeseries(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "host1", "/tmp/project", "python train.py", "train")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := seedJobRunSpec(database, jobID, seededRunSpec{project: "proj-a", gpuClass: "A100"}); err != nil {
		t.Fatalf("seed run1 spec: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning run1: %v", err)
	}
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID run1: %v", err)
	}
	run1ID := *job.LatestRunID
	meta1 := &JobMetadata{Resource: &ResourceUsage{PeakRSSKB: int64Ptr(111)}, CPU: &JobCPUStats{Mean: float64Ptr(10.5)}}
	if err := SetJobMetadata(database, jobID, meta1); err != nil {
		t.Fatalf("SetJobMetadata run1: %v", err)
	}
	if err := InsertTimeseries(database, jobID, []TimeseriesSample{{Ts: 1000, CPUPct: 11}}); err != nil {
		t.Fatalf("InsertTimeseries run1: %v", err)
	}
	if err := RecordCompletionByID(database, jobID, 0, job.StartTime+10); err != nil {
		t.Fatalf("RecordCompletionByID run1: %v", err)
	}

	if err := RequeueByID(database, jobID); err != nil {
		t.Fatalf("RequeueByID: %v", err)
	}
	if err := seedJobRunSpec(database, jobID, seededRunSpec{
		command:  "python train_v2.py",
		project:  "proj-b",
		gpuClass: "H100",
	}); err != nil {
		t.Fatalf("seed run2 spec: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning run2: %v", err)
	}
	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID run2: %v", err)
	}
	run2ID := *job.LatestRunID
	meta2 := &JobMetadata{Resource: &ResourceUsage{PeakRSSKB: int64Ptr(222)}, CPU: &JobCPUStats{Mean: float64Ptr(20.5)}}
	if err := SetJobMetadata(database, jobID, meta2); err != nil {
		t.Fatalf("SetJobMetadata run2: %v", err)
	}
	if err := InsertTimeseries(database, jobID, []TimeseriesSample{{Ts: 2000, CPUPct: 22}}); err != nil {
		t.Fatalf("InsertTimeseries run2: %v", err)
	}
	if err := RecordCompletionByID(database, jobID, 3, job.StartTime+20); err != nil {
		t.Fatalf("RecordCompletionByID run2: %v", err)
	}

	runs, err := ListTrainingJobRuns(database, 0)
	if err != nil {
		t.Fatalf("ListTrainingJobRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("training run count = %d, want 2", len(runs))
	}
	if runs[0].RunID != run1ID || runs[0].Project != "proj-a" || runs[0].GPUClass != "A100" || runs[0].Command != "python train.py" {
		t.Fatalf("run1 training record = %+v", runs[0])
	}
	if runs[0].Metadata == nil || runs[0].Metadata.Resource == nil || runs[0].Metadata.Resource.PeakRSSKB == nil || *runs[0].Metadata.Resource.PeakRSSKB != 111 {
		t.Fatalf("run1 metadata = %+v", runs[0].Metadata)
	}
	if runs[1].RunID != run2ID || runs[1].Project != "proj-b" || runs[1].GPUClass != "H100" || runs[1].Command != "python train_v2.py" {
		t.Fatalf("run2 training record = %+v", runs[1])
	}
	if runs[1].ExitCode != 3 {
		t.Fatalf("run2 exit_code = %d, want 3", runs[1].ExitCode)
	}

	ts1, err := GetTimeseriesByRun(database, run1ID)
	if err != nil {
		t.Fatalf("GetTimeseriesByRun run1: %v", err)
	}
	if len(ts1) != 1 || ts1[0].JobRunID == nil || *ts1[0].JobRunID != run1ID || ts1[0].Ts != 1000 {
		t.Fatalf("run1 timeseries = %+v", ts1)
	}
	ts2, err := GetTimeseriesByRun(database, run2ID)
	if err != nil {
		t.Fatalf("GetTimeseriesByRun run2: %v", err)
	}
	if len(ts2) != 1 || ts2[0].JobRunID == nil || *ts2[0].JobRunID != run2ID || ts2[0].Ts != 2000 {
		t.Fatalf("run2 timeseries = %+v", ts2)
	}
}

func TestSetJobPlacementMetaDoesNotCreateRunBeforeStart(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "host1", "/tmp/project", "python train.py", "train")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := SetJobPlacementMeta(database, jobID, &PlacementMeta{SelectedScore: 1.5}); err != nil {
		t.Fatalf("SetJobPlacementMeta: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LatestRunID != nil {
		t.Fatalf("latest_run_id = %v, want nil before execution starts", job.LatestRunID)
	}
}

func TestSetJobPlacementReasonsDoesNotCreateRunBeforeStart(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "host1", "/tmp/project", "python train.py", "train")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := SetJobPlacementReasons(database, jobID, []string{"manually moved to unplaced queue"}); err != nil {
		t.Fatalf("SetJobPlacementReasons: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LatestRunID != nil {
		t.Fatalf("latest_run_id = %v, want nil before execution starts", job.LatestRunID)
	}
}

func TestOpenBackfillsLegacyTerminalJobsIntoRuns(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "weft-legacy-db-*.db")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	tmpFile.Close()

	cleanup := SetDBPath(tmpFile.Name())
	t.Cleanup(func() {
		cleanup()
		os.Remove(tmpFile.Name())
	})

	legacyDB, err := sql.Open("sqlite", tmpFile.Name())
	if err != nil {
		t.Fatalf("sql.Open legacy db: %v", err)
	}

	legacySchema := `
	CREATE TABLE jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		host TEXT NOT NULL,
		session_name TEXT,
		working_dir TEXT NOT NULL,
		command TEXT NOT NULL,
		description TEXT,
		job_metadata TEXT,
		tags TEXT,
		start_time INTEGER,
		end_time INTEGER,
		exit_code INTEGER,
		status TEXT NOT NULL DEFAULT 'running',
		tombstoned INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE job_timeseries (
		job_id INTEGER NOT NULL,
		ts INTEGER NOT NULL,
		cpu_pct INTEGER,
		rss_kb INTEGER,
		gpu_mib INTEGER,
		disk_free_bytes INTEGER,
		disk_total_bytes INTEGER,
		host_rss_kb INTEGER,
		host_mem_total_kb INTEGER,
		gpu_util_pct INTEGER,
		gpu_mem_used_mib INTEGER,
		gpu_mem_total_mib INTEGER,
		tenant TEXT,
		PRIMARY KEY (job_id, ts)
	);
	CREATE TABLE artifacts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id INTEGER NOT NULL,
		name TEXT,
		path TEXT NOT NULL,
		stored_path TEXT NOT NULL,
		size_bytes INTEGER NOT NULL DEFAULT 0,
		sha256 TEXT,
		created_at INTEGER NOT NULL
	);
	`
	if _, err := legacyDB.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	metadataJSON := `{"resource":{"peak_rss_kb":4096,"max_gpu_mem_mib":8192},"cpu":{"mean":42.5}}`
	jobResult, err := legacyDB.Exec(
		`INSERT INTO jobs (host, session_name, working_dir, command, description, job_metadata, tags, start_time, end_time, exit_code, status, tombstoned)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		"host1", "", "/tmp/project", "python train.py", "legacy train", metadataJSON, `["train"]`, 100, 130, 0, StatusCompleted,
	)
	if err != nil {
		t.Fatalf("insert legacy job: %v", err)
	}
	jobID, err := jobResult.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}

	if _, err := legacyDB.Exec(
		`INSERT INTO job_timeseries (
			job_id, ts, cpu_pct, rss_kb, gpu_mib, disk_free_bytes, disk_total_bytes,
			host_rss_kb, host_mem_total_kb, gpu_util_pct, gpu_mem_used_mib, gpu_mem_total_mib, tenant
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		jobID, 105, 55, 2048, 4096, 0, 0, 8192, 16384, 77, 4096, 8192, "multi",
	); err != nil {
		t.Fatalf("insert legacy timeseries: %v", err)
	}
	if _, err := legacyDB.Exec(
		`INSERT INTO artifacts (job_id, name, path, stored_path, size_bytes, sha256, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		jobID, "model", "output/model.bin", "/tmp/store/model.bin", 1234, "abc", 131,
	); err != nil {
		t.Fatalf("insert legacy artifact: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	database, err := Open()
	if err != nil {
		t.Fatalf("Open migrated db: %v", err)
	}
	defer database.Close()

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || job.LatestRunID == nil {
		t.Fatalf("latest_run_id not backfilled: %+v", job)
	}

	run, err := GetJobRunByID(database, *job.LatestRunID)
	if err != nil {
		t.Fatalf("GetJobRunByID: %v", err)
	}
	if run.JobID != jobID || run.Status != StatusCompleted || run.Command != "python train.py" || run.Description != "legacy train" {
		t.Fatalf("backfilled run = %+v", run)
	}
	if run.StartTime != 100 || run.EndTime == nil || *run.EndTime != 130 || run.ExitCode == nil || *run.ExitCode != 0 {
		t.Fatalf("backfilled run timing/exit = %+v", run)
	}
	if run.Metadata == nil || run.Metadata.Resource == nil || run.Metadata.Resource.PeakRSSKB == nil || *run.Metadata.Resource.PeakRSSKB != 4096 {
		t.Fatalf("backfilled run metadata = %+v", run.Metadata)
	}

	ts, err := GetTimeseriesByRun(database, *job.LatestRunID)
	if err != nil {
		t.Fatalf("GetTimeseriesByRun: %v", err)
	}
	if len(ts) != 1 || ts[0].JobRunID == nil || *ts[0].JobRunID != *job.LatestRunID {
		t.Fatalf("backfilled timeseries = %+v", ts)
	}

	arts, err := ListArtifactsByRun(database, *job.LatestRunID)
	if err != nil {
		t.Fatalf("ListArtifactsByRun: %v", err)
	}
	if len(arts) != 1 || arts[0].JobRunID == nil || *arts[0].JobRunID != *job.LatestRunID {
		t.Fatalf("backfilled artifacts = %+v", arts)
	}

	runs, err := ListTrainingJobRuns(database, 0)
	if err != nil {
		t.Fatalf("ListTrainingJobRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].RunID != *job.LatestRunID || runs[0].JobID != jobID {
		t.Fatalf("training export rows = %+v", runs)
	}

	if err := database.Close(); err != nil {
		t.Fatalf("close migrated db: %v", err)
	}

	database, err = Open()
	if err != nil {
		t.Fatalf("reopen migrated db: %v", err)
	}
	defer database.Close()

	var runCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM job_runs WHERE job_id = ?`, jobID).Scan(&runCount); err != nil {
		t.Fatalf("count job_runs: %v", err)
	}
	if runCount != 1 {
		t.Fatalf("job_runs count after reopen = %d, want 1", runCount)
	}
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
