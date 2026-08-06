package ops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/artifactspec"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/queuefile"
	"github.com/osteele/weft/internal/ssh"
)

// AppendJobToQueue adds an existing job to the remote queue.
func AppendJobToQueue(job *db.Job, timeout time.Duration) error {
	return AppendJobToQueueWithSource(job, timeout, "")
}

// AppendJobToQueueWithSource adds an existing job to the remote queue and
// includes an optional source snapshot hash for provenance checks.
func AppendJobToQueueWithSource(job *db.Job, timeout time.Duration, sourceSHA256 string) error {
	return AppendJobToQueueWithSourceAndR2(job, timeout, sourceSHA256, "")
}

// AppendJobToQueueWithSourceAndR2 adds an existing job to the remote queue
// with an optional source SHA and an optional R2 source-tarball key. When
// sourceR2Key is non-empty, the runner switches to R2-isolated mode for this
// job (Layer D fallback): it downloads the tarball, extracts into a per-job
// dir, and skips the marker check.
func AppendJobToQueueWithSourceAndR2(job *db.Job, timeout time.Duration, sourceSHA256, sourceR2Key string) error {
	var runID int64
	if job.LatestRunID != nil {
		runID = *job.LatestRunID
	}
	entry := opsqueue.QueueEntry{
		JobID:        job.ID,
		RunID:        runID,
		WorkingDir:   job.WorkingDir,
		Command:      job.Command,
		Description:  job.Description,
		SourceSHA256: sourceSHA256,
		SourceR2Key:  sourceR2Key,
		EnvVars:      job.EnvVars,
		DepSpec:      job.DepSpec,
		CPUAllotment: job.CPUAllotment,
		GPU:          job.GPU,
		GPUClass:     job.GPUClass,
		GPUCount:     job.RequestedGPUCount(),
		GPUMemGB:     job.GPUMemGB,
		Interconnect: job.RequestedInterconnect(),
		CPUCores:     job.RequestedCPUCores(),
		Tags:         job.Tags,
		OutputDirs:   job.OutputDirs,
		Outputs:      job.Outputs,
		Produces:     job.Produces,
		Needs:        job.Needs,
	}
	addCmd := opsqueue.NewAddCommand(entry)
	opts := opsqueue.AppendCommandOptions{Timeout: timeout}
	return opsqueue.AppendCommand(job.Host, addCmd, opts)
}

// UpdateQueueEntryParams contains parameters for updating an existing queue entry
type UpdateQueueEntryParams struct {
	Host    string
	Job     *db.Job
	EnvVars []string
	DepSpec string
	Timeout time.Duration
}

// QueueJobParams contains parameters for queueing a new job
type QueueJobParams struct {
	Host         string
	WorkingDir   string
	Command      string
	Description  string
	Project      string
	EnvVars      []string
	Tags         []string
	GPU          string // Explicit GPU setting; if empty, extracted from EnvVars
	GPUClass     string // GPU class name (e.g., "A100") — resolved to device at runtime
	GPUCount     int    // Exact GPU count requested on this host
	GPUMemGB     *int   // GPU memory reservation in GB per device
	Interconnect string
	CPUCores     int
	GPUMemMaxGB  *int // Legacy GPU memory upper metadata; ignored by placement
	DepSpec      string
	CPUAllotment *int
	OutputDirs   []string // convention-based output directories from .weft.toml
	Inputs       []string // Data asset refs the job reads (e.g., "hf:meta-llama/Llama-3-8B")
	// BestEffortInputs are inputs that came only from source/command auto-detection.
	// Failed cloud prewarm for these refs warns and continues.
	BestEffortInputs []string
	Outputs          []string // Data asset refs the job produces (e.g., "checkpoint:llama-ft-v1")
	Produces         []string // Artifact specs this job produces (e.g., "output/model.pt" or "output/model.pt:100")
	Needs            []string // Artifact specs this job needs (e.g., "output/model.pt:100")
	Disk             *db.JobDiskMetadata
	CLIOverrides     *db.CLIResourceOverrides
	MaxComputeCap    string
	SubmitToken      string
	// Metadata is persisted to job_attempts.job_metadata as part of the same
	// RecordQueuedJob call. Writing metadata inside RecordQueuedJob (rather
	// than the caller doing a follow-up SetJobMetadata) avoids a race where a
	// concurrent sync worker picks up the newly-visible job before CloudNeeds
	// / CloudAfter have been written, silently skipping artifact staging.
	Metadata *db.JobMetadata
}

// RecordQueuedJob records a job in the local database with "queued" status.
// This is DB-only — no SSH or remote operations are performed.
// The job will be pushed to the remote host by SyncHost on the next sync cycle.
func RecordQueuedJob(database *sql.DB, params QueueJobParams) (int64, error) {
	return RecordQueuedJobContext(context.Background(), database, params)
}

// RecordQueuedJobContext records a job like RecordQueuedJob, but honors ctx
// while waiting for the SQLite writer lock.
func RecordQueuedJobContext(ctx context.Context, database *sql.DB, params QueueJobParams) (int64, error) {
	return recordQueuedJob(ctx, database, 0, params, false, false)
}

// RecordDraftJob records a job and all submit metadata atomically in draft status.
func RecordDraftJob(database *sql.DB, params QueueJobParams) (int64, error) {
	return recordQueuedJob(context.Background(), database, 0, params, false, true)
}

// MirrorQueuedJobWithID records or updates a queued job using an explicit ID.
func MirrorQueuedJobWithID(database *sql.DB, jobID int64, params QueueJobParams) error {
	_, err := recordQueuedJob(context.Background(), database, jobID, params, true, false)
	return err
}

func recordQueuedJob(ctx context.Context, database *sql.DB, explicitJobID int64, params QueueJobParams, explicitID, draft bool) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(params.Host) == "" && strings.TrimSpace(params.WorkingDir) == "" {
		return 0, fmt.Errorf("cloud jobs require a local working directory; run from a configured automap directory or pass --dir")
	}
	if err := artifactspec.ValidateNeedsSpecs(params.Needs); err != nil {
		return 0, fmt.Errorf("needs: %w", err)
	}
	if err := validateNamedAssetRefs(database, params.Inputs, params.Needs); err != nil {
		return 0, err
	}
	if err := dataloc.ValidateExplicitHFInputs(params.Inputs, params.BestEffortInputs); err != nil {
		return 0, fmt.Errorf("inputs: %w", err)
	}

	// Extract GPU from env vars if not explicitly set
	gpu := params.GPU
	if gpu == "" {
		for _, ev := range params.EnvVars {
			if strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") {
				gpu = strings.TrimPrefix(ev, "CUDA_VISIBLE_DEVICES=")
				break
			}
		}
	}

	// Apply default GPU memory reservation when GPU is involved but no explicit reservation.
	gpuMemGB := params.GPUMemGB
	if gpuMemGB == nil && (gpu != "" || params.GPUClass != "") {
		defaultMem := opsqueue.DefaultGPUMemGB
		gpuMemGB = &defaultMem
	}

	project, err := db.NormalizeProjectName(params.Project, params.WorkingDir, params.Command)
	if err != nil {
		return 0, fmt.Errorf("resolve project: %w", err)
	}
	metadata := mergeJobMetadata(params.Metadata, params.Disk, params.BestEffortInputs)
	submitToken := strings.TrimSpace(params.SubmitToken)

	return db.RetryOnDatabaseLockedValue(ctx, "record queued job", func() (int64, error) {
		tx, err := database.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()

		jobID, err := recordQueuedJobTx(tx, explicitJobID, params, explicitID, draft, gpu, gpuMemGB, project, metadata, submitToken)
		if err != nil {
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		committed = true
		return jobID, nil
	})
}

func validateNamedAssetRefs(database *sql.DB, inputs, needs []string) error {
	sources := make(map[string]string)
	add := func(name, source string) {
		if strings.TrimSpace(name) != "" {
			if prev, ok := sources[name]; ok && prev != source {
				sources[name] = "inputs/needs"
				return
			}
			sources[name] = source
		}
	}
	for _, input := range inputs {
		asset, ok := dataloc.ParseAssetRef(input)
		if ok && asset.Kind == dataloc.AssetNamed {
			add(asset.ID, "inputs")
		}
	}
	for _, need := range needs {
		spec, err := artifactspec.ParseNeedsSpec(need)
		if err != nil {
			continue
		}
		if spec.IsAsset() {
			add(spec.AssetName, "needs")
		}
	}
	for name, source := range sources {
		if _, err := db.GetNamedAssetByName(database, name); err != nil {
			if errors.Is(err, db.ErrNamedAssetNotFound) {
				hint := fmt.Sprintf("publish it first with `weft data publish <path> --name %s`, or use an existing asset:NAME", name)
				if strings.ContainsAny(name, `/\`) {
					hint = "asset: expects a published asset name, not a file path; publish the file first with `weft data publish <path> --name <name>`, then use asset:<name>"
				}
				return fmt.Errorf("%s: named asset %q not found; %s", source, name, hint)
			}
			return fmt.Errorf("%s: lookup named asset %q: %w", source, name, err)
		}
	}
	return nil
}

func recordQueuedJobTx(tx *sql.Tx, explicitJobID int64, params QueueJobParams, explicitID, draft bool, gpu string, gpuMemGB *int, project string, metadata *db.JobMetadata, submitToken string) (int64, error) {
	if submitToken != "" {
		if jobID, ok, err := db.FindJobIDBySubmitToken(tx, submitToken); err != nil {
			return 0, fmt.Errorf("lookup submit token: %w", err)
		} else if ok {
			return jobID, nil
		}
	}

	// Record job with queued status
	var jobID int64
	if draft {
		if explicitID {
			return 0, fmt.Errorf("record draft job with explicit ID is not supported")
		}
		var err error
		jobID, err = db.RecordDraftJobWithGPUTx(tx, params.Host, params.WorkingDir, params.Command, params.Description, gpu)
		if err != nil {
			return 0, fmt.Errorf("record draft job: %w", err)
		}
	} else if explicitID {
		jobID = explicitJobID
		if err := db.RecordQueuedWithGPUAndIDTx(tx, jobID, params.Host, params.WorkingDir, params.Command, params.Description, gpu); err != nil {
			return 0, fmt.Errorf("record job: %w", err)
		}
	} else {
		var err error
		jobID, err = db.RecordQueuedWithGPUTx(tx, params.Host, params.WorkingDir, params.Command, params.Description, gpu)
		if err != nil {
			return 0, fmt.Errorf("record job: %w", err)
		}
	}
	if err := db.SetJobEnvVars(tx, jobID, params.EnvVars); err != nil {
		return 0, fmt.Errorf("record env vars: %w", err)
	}
	if len(params.Tags) > 0 {
		if err := db.SetJobTags(tx, jobID, params.Tags); err != nil {
			return 0, fmt.Errorf("record tags: %w", err)
		}
	}
	if err := db.SetJobDepSpec(tx, jobID, params.DepSpec); err != nil {
		return 0, fmt.Errorf("record dependencies: %w", err)
	}
	if project != "" {
		if err := db.SetJobProject(tx, jobID, project); err != nil {
			return 0, fmt.Errorf("record project: %w", err)
		}
	}
	if params.CPUAllotment != nil {
		if err := db.SetJobCPUAllotment(tx, jobID, params.CPUAllotment); err != nil {
			return 0, fmt.Errorf("record CPU allotment: %w", err)
		}
	}
	if gpuMemGB != nil {
		if err := db.SetJobGPUMemGB(tx, jobID, gpuMemGB); err != nil {
			return 0, fmt.Errorf("record GPU memory: %w", err)
		}
	}
	if params.GPUMemMaxGB != nil {
		if err := db.SetJobGPUMemMaxGB(tx, jobID, params.GPUMemMaxGB); err != nil {
			return 0, fmt.Errorf("record legacy GPU memory upper metadata: %w", err)
		}
	}
	if params.GPUClass != "" {
		if err := db.SetJobGPUClass(tx, jobID, params.GPUClass); err != nil {
			return 0, fmt.Errorf("record GPU class: %w", err)
		}
	}
	if params.CLIOverrides != nil {
		if err := db.SetJobCLIResourceOverrides(tx, jobID, params.CLIOverrides); err != nil {
			return 0, fmt.Errorf("record CLI resource overrides: %w", err)
		}
	}
	if params.MaxComputeCap != "" {
		if err := db.SetJobMaxComputeCap(tx, jobID, params.MaxComputeCap); err != nil {
			return 0, fmt.Errorf("record max compute cap: %w", err)
		}
	}
	if submitToken != "" {
		if err := db.SetJobSubmitToken(tx, jobID, submitToken); err != nil {
			return 0, fmt.Errorf("record submit token: %w", err)
		}
	}
	if len(params.Inputs) > 0 {
		if err := db.SetJobInputs(tx, jobID, params.Inputs); err != nil {
			return 0, fmt.Errorf("record inputs: %w", err)
		}
	}
	if len(params.Outputs) > 0 {
		if err := db.SetJobOutputs(tx, jobID, params.Outputs); err != nil {
			return 0, fmt.Errorf("record outputs: %w", err)
		}
	}
	if len(params.OutputDirs) > 0 {
		if err := db.SetJobOutputDirs(tx, jobID, params.OutputDirs); err != nil {
			return 0, fmt.Errorf("record output dirs: %w", err)
		}
	}
	if len(params.Produces) > 0 {
		if err := db.SetJobProduces(tx, jobID, params.Produces); err != nil {
			return 0, fmt.Errorf("record produces: %w", err)
		}
	}
	if len(params.Needs) > 0 {
		if err := db.SetJobNeeds(tx, jobID, params.Needs); err != nil {
			return 0, fmt.Errorf("record needs: %w", err)
		}
	}
	if metadata != nil {
		if err := db.SetJobMetadata(tx, jobID, metadata); err != nil {
			return 0, fmt.Errorf("record job metadata: %w", err)
		}
	}

	return jobID, nil
}

func mergeJobMetadata(meta *db.JobMetadata, disk *db.JobDiskMetadata, bestEffortInputs []string) *db.JobMetadata {
	if meta == nil && disk == nil && len(bestEffortInputs) == 0 {
		return nil
	}
	if meta == nil {
		meta = &db.JobMetadata{}
	}
	if disk != nil {
		meta.Disk = disk
	}
	if len(bestEffortInputs) > 0 {
		meta.BestEffortInputs = append([]string(nil), bestEffortInputs...)
	}
	return meta
}

// QueueJob creates a job record and adds it to the remote queue.
func QueueJob(database *sql.DB, params QueueJobParams, opts ExecuteOptions) (Result, error) {
	jobID, err := RecordQueuedJob(database, params)
	if err != nil {
		return Result{}, err
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return Result{}, fmt.Errorf("get job: %w", err)
	}
	return submitRecordedQueuedJob(database, job, opts)
}

// SubmitRecordedQueuedJob submits an already-recorded queued job to its remote backend.
func SubmitRecordedQueuedJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	return submitRecordedQueuedJob(database, job, opts)
}

func submitRecordedQueuedJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	if job == nil {
		return Result{}, fmt.Errorf("job is nil")
	}
	backend, err := ResolveBackend(job.Host, opts.Timeout)
	if err != nil {
		if ssh.IsConnectionError(err.Error()) {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    job.ID,
				Message:  fmt.Sprintf("Host %s unreachable, job %s will start on next sync", job.Host, ids.FormatJobID(job.ID)),
			}, nil
		}
		return Result{}, err
	}
	if err := db.SetJobBackend(database, job.ID, backend); err != nil {
		return Result{}, fmt.Errorf("set job backend: %w", err)
	}

	if backend == db.BackendSlurm {
		outcome, err := requestJobStatus(database, job, db.StatusQueued, opts)
		if err != nil {
			return Result{}, err
		}
		if !outcome.hostAvailable {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    job.ID,
				Message:  fmt.Sprintf("Host %s unreachable, job %s will start on next sync", job.Host, ids.FormatJobID(job.ID)),
			}, nil
		}
		if !outcome.resolved {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    job.ID,
				Message:  fmt.Sprintf("Host %s unreachable, job %s will start on next sync", job.Host, ids.FormatJobID(job.ID)),
			}, nil
		}
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %s submitted", ids.FormatJobID(job.ID)),
		}, nil
	}

	if err := AppendJobToQueue(job, opts.Timeout); err != nil {
		if ssh.IsConnectionError(err.Error()) {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    job.ID,
				Message:  fmt.Sprintf("Job %s queued locally (will append to queue when host is online)", ids.FormatJobID(job.ID)),
			}, nil
		}
		return Result{}, err
	}
	if err := db.UpdateLastSyncedStatus(database, job.ID, db.StatusQueued); err != nil {
		return Result{}, err
	}
	if err := db.SetQueuedAtNow(database, job.ID); err != nil {
		return Result{}, err
	}

	return Result{
		Success: true,
		JobID:   job.ID,
		Message: fmt.Sprintf("Job %s added to queue", ids.FormatJobID(job.ID)),
	}, nil
}

// UpdateQueueEntry updates an existing job's entry in the remote queue file.
func UpdateQueueEntry(params UpdateQueueEntryParams) error {
	if params.Job == nil {
		return fmt.Errorf("job is nil")
	}

	entry := queueEntryForJob(params.Job, params.EnvVars, params.DepSpec)
	if err := writeQueueJobFile(params.Host, entry, params.Timeout); err != nil {
		if isQueueConnectionError(err) {
			return fmt.Errorf("host unreachable")
		}
		return fmt.Errorf("update queue entry: %w", err)
	}
	return nil
}

// UpdateQueuedJobEntry refreshes a queued job entry on the remote host.
func UpdateQueuedJobEntry(job *db.Job, envVars []string, depSpec string) error {
	if job == nil {
		return fmt.Errorf("job is nil")
	}

	entryJob := &db.Job{
		ID:           job.ID,
		Host:         job.Host,
		WorkingDir:   job.WorkingDir,
		Command:      job.Command,
		Description:  job.Description,
		EnvVars:      append([]string(nil), job.EnvVars...),
		DepSpec:      job.DepSpec,
		CPUAllotment: job.CPUAllotment,
		GPU:          job.GPU,
		GPUClass:     job.GPUClass,
		GPUMemGB:     job.GPUMemGB,
		Tags:         append([]string(nil), job.Tags...),
		OutputDirs:   append([]string(nil), job.OutputDirs...),
		Outputs:      append([]string(nil), job.Outputs...),
		Produces:     append([]string(nil), job.Produces...),
		Needs:        append([]string(nil), job.Needs...),
	}

	needEnv := len(envVars) == 0
	needDir := entryJob.WorkingDir == ""
	needCmd := entryJob.Command == ""

	if needEnv || needDir || needCmd {
		entry, err := queuefile.FetchEntry(job.Host, job.ID)
		if err != nil {
			return err
		}
		if needDir && entry.WorkingDir != "" {
			entryJob.WorkingDir = entry.WorkingDir
		}
		if needCmd && entry.Command != "" {
			entryJob.Command = entry.Command
		}
		if entryJob.Description == "" && entry.Description != "" {
			entryJob.Description = entry.Description
		}
		if needEnv {
			envVars = entry.EnvVars
		}
	}

	return UpdateQueueEntry(UpdateQueueEntryParams{
		Host:    job.Host,
		Job:     entryJob,
		EnvVars: envVars,
		DepSpec: depSpec,
	})
}
