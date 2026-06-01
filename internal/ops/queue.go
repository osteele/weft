package ops

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/queuefile"
	"github.com/osteele/weft/internal/ssh"
)

// Re-export queue types from opsqueue.
const (
	QueueDir        = opsqueue.QueueDir
	DefaultGPUMemGB = opsqueue.DefaultGPUMemGB
)

type QueueEntry = opsqueue.QueueEntry
type AppendQueueEntryOptions = opsqueue.AppendQueueEntryOptions
type QueueAppendError = opsqueue.QueueAppendError

func AppendQueueEntry(host string, entry QueueEntry, opts AppendQueueEntryOptions) error {
	return opsqueue.AppendQueueEntry(host, entry, opts)
}

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
	entry := QueueEntry{
		JobID:        job.ID,
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
		GPUMemGB:     job.GPUMemGB,
		Tags:         job.Tags,
		OutputDirs:   job.OutputDirs,
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
	GPUMemGB     *int   // GPU memory reservation in GB per device
	GPUMemMaxGB  *int   // Legacy GPU memory upper metadata; ignored by placement
	DepSpec      string
	CPUAllotment *int
	OutputDirs   []string // convention-based output directories from .weft.toml
	Inputs       []string // Data asset refs the job reads (e.g., "hf:meta-llama/Llama-3-8B")
	Outputs      []string // Data asset refs the job produces (e.g., "checkpoint:llama-ft-v1")
	Produces     []string // Artifact specs this job produces (e.g., "output/model.pt" or "output/model.pt:100")
	Needs        []string // Artifact specs this job needs (e.g., "output/model.pt:100")
	Disk         *db.JobDiskMetadata
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
	return recordQueuedJob(database, 0, params, false)
}

// MirrorQueuedJobWithID records or updates a queued job using an explicit ID.
func MirrorQueuedJobWithID(database *sql.DB, jobID int64, params QueueJobParams) error {
	_, err := recordQueuedJob(database, jobID, params, true)
	return err
}

func recordQueuedJob(database *sql.DB, explicitJobID int64, params QueueJobParams, explicitID bool) (int64, error) {
	if strings.TrimSpace(params.Host) == "" && strings.TrimSpace(params.WorkingDir) == "" {
		return 0, fmt.Errorf("cloud jobs require a local working directory; run from a configured automap directory or pass --dir")
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
		defaultMem := DefaultGPUMemGB
		gpuMemGB = &defaultMem
	}

	// Record job with queued status
	var jobID int64
	if explicitID {
		jobID = explicitJobID
		if err := db.RecordQueuedWithGPUAndID(database, jobID, params.Host, params.WorkingDir, params.Command, params.Description, gpu); err != nil {
			return 0, fmt.Errorf("record job: %w", err)
		}
	} else {
		var err error
		jobID, err = db.RecordQueuedWithGPU(database, params.Host, params.WorkingDir, params.Command, params.Description, gpu)
		if err != nil {
			return 0, fmt.Errorf("record job: %w", err)
		}
	}
	if err := db.SetJobEnvVars(database, jobID, params.EnvVars); err != nil {
		if !explicitID {
			db.DeleteJob(database, jobID)
		}
		return 0, fmt.Errorf("record env vars: %w", err)
	}
	if len(params.Tags) > 0 {
		if err := db.SetJobTags(database, jobID, params.Tags); err != nil {
			if !explicitID {
				db.DeleteJob(database, jobID)
			}
			return 0, fmt.Errorf("record tags: %w", err)
		}
	}
	if err := db.SetJobDepSpec(database, jobID, params.DepSpec); err != nil {
		return 0, fmt.Errorf("record dependencies: %w", err)
	}
	project, err := db.NormalizeProjectName(params.Project, params.WorkingDir, params.Command)
	if err != nil {
		return 0, fmt.Errorf("resolve project: %w", err)
	}
	if project != "" {
		if err := db.SetJobProject(database, jobID, project); err != nil {
			return 0, fmt.Errorf("record project: %w", err)
		}
	}
	if params.CPUAllotment != nil {
		if err := db.SetJobCPUAllotment(database, jobID, params.CPUAllotment); err != nil {
			if !explicitID {
				db.DeleteJob(database, jobID)
			}
			return 0, fmt.Errorf("record CPU allotment: %w", err)
		}
	}
	if gpuMemGB != nil {
		if err := db.SetJobGPUMemGB(database, jobID, gpuMemGB); err != nil {
			if !explicitID {
				db.DeleteJob(database, jobID)
			}
			return 0, fmt.Errorf("record GPU memory: %w", err)
		}
	}
	if params.GPUMemMaxGB != nil {
		if err := db.SetJobGPUMemMaxGB(database, jobID, params.GPUMemMaxGB); err != nil {
			if !explicitID {
				db.DeleteJob(database, jobID)
			}
			return 0, fmt.Errorf("record legacy GPU memory upper metadata: %w", err)
		}
	}
	if params.GPUClass != "" {
		if err := db.SetJobGPUClass(database, jobID, params.GPUClass); err != nil {
			if !explicitID {
				db.DeleteJob(database, jobID)
			}
			return 0, fmt.Errorf("record GPU class: %w", err)
		}
	}
	if len(params.Inputs) > 0 {
		if err := db.SetJobInputs(database, jobID, params.Inputs); err != nil {
			if !explicitID {
				db.DeleteJob(database, jobID)
			}
			return 0, fmt.Errorf("record inputs: %w", err)
		}
	}
	if len(params.Outputs) > 0 {
		if err := db.SetJobOutputs(database, jobID, params.Outputs); err != nil {
			if !explicitID {
				db.DeleteJob(database, jobID)
			}
			return 0, fmt.Errorf("record outputs: %w", err)
		}
	}
	if len(params.OutputDirs) > 0 {
		if err := db.SetJobOutputDirs(database, jobID, params.OutputDirs); err != nil {
			if !explicitID {
				db.DeleteJob(database, jobID)
			}
			return 0, fmt.Errorf("record output dirs: %w", err)
		}
	}
	if len(params.Produces) > 0 {
		if err := db.SetJobProduces(database, jobID, params.Produces); err != nil {
			if !explicitID {
				db.DeleteJob(database, jobID)
			}
			return 0, fmt.Errorf("record produces: %w", err)
		}
	}
	if len(params.Needs) > 0 {
		if err := db.SetJobNeeds(database, jobID, params.Needs); err != nil {
			if !explicitID {
				db.DeleteJob(database, jobID)
			}
			return 0, fmt.Errorf("record needs: %w", err)
		}
	}
	metadata := mergeJobMetadata(params.Metadata, params.Disk)
	if metadata != nil {
		if err := db.SetJobMetadata(database, jobID, metadata); err != nil {
			if !explicitID {
				db.DeleteJob(database, jobID)
			}
			return 0, fmt.Errorf("record job metadata: %w", err)
		}
	}

	return jobID, nil
}

func mergeJobMetadata(meta *db.JobMetadata, disk *db.JobDiskMetadata) *db.JobMetadata {
	if meta == nil && disk == nil {
		return nil
	}
	if meta == nil {
		meta = &db.JobMetadata{}
	}
	if disk != nil {
		meta.Disk = disk
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
		ID:          job.ID,
		Host:        job.Host,
		WorkingDir:  job.WorkingDir,
		Command:     job.Command,
		Description: job.Description,
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
