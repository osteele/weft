package ops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/artifactspec"
	"github.com/osteele/weft/internal/compat"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/ssh"
)

// AppendJobToQueue adds an existing job to the remote queue.
func AppendJobToQueue(database *sql.DB, job *db.Job, timeout time.Duration) error {
	return AppendJobToQueueWithSource(database, job, timeout, "")
}

// AppendJobToQueueWithSource adds an existing job to the remote queue and
// includes an optional source snapshot hash for provenance checks.
func AppendJobToQueueWithSource(database *sql.DB, job *db.Job, timeout time.Duration, sourceSHA256 string) error {
	return AppendJobToQueueWithSourceAndR2(database, job, timeout, sourceSHA256, "")
}

// AppendJobToQueueWithSourceAndR2 adds an existing job to the remote queue
// with an optional source SHA and an optional R2 source-tarball key. When
// sourceR2Key is non-empty, the runner switches to R2-isolated mode for this
// job (Layer D fallback): it downloads the tarball, extracts into a per-job
// dir, and skips the marker check.
func AppendJobToQueueWithSourceAndR2(database *sql.DB, job *db.Job, timeout time.Duration, sourceSHA256, sourceR2Key string) error {
	state, err := fetchRemoteRunnerState(job.Host, timeout)
	if err != nil {
		return fmt.Errorf("read queue runner protocol: %w", err)
	}
	recordHostAgentRuntimeObservation(database, job.Host, state, time.Now())
	return appendJobToQueueWithSourceManifest(database, job, timeout, sourceSHA256, sourceR2Key, nil, state, defaultR2Client)
}

func appendJobToQueueWithSourceManifest(database *sql.DB, job *db.Job, timeout time.Duration, sourceSHA256, sourceR2Key string, sourceManifest *opsqueue.SourceManifest, state *opsqueue.RunnerState, getR2Client func() (*r2.Client, error)) error {
	if err := queueProtocolCompatibilityError(job.Host, state); err != nil {
		return err
	}
	pinned, ok, err := pinnedQueueSourceManifest(job)
	if err != nil {
		return err
	}
	if ok {
		sourceManifest = pinned
		sourceSHA256 = pinned.SHA256
		// Older runners ignore source_manifest. Point their already-supported
		// source_r2_key at the closure receipt JSON so they reject extraction
		// instead of silently running an incomplete single-root snapshot.
		sourceR2Key = dataplane.SourceClosureReceiptV2(pinned.SHA256)
	}
	var runID int64
	if job.LatestRunID != nil {
		runID = *job.LatestRunID
	}
	payloads, err := queuePayloadsForJob(database, job.ID)
	if err != nil {
		return fmt.Errorf("list job payloads: %w", err)
	}
	artifactNeeds := []opsqueue.ArtifactNeed(nil)
	if hostUsesR2Queue(job.Host) {
		artifactNeeds, err = resolveR2QueueNeeds(database, job, getR2Client)
		if err != nil {
			return err
		}
	} else if sourceManifest != nil || sourceR2Key != "" {
		artifactNeeds, err = resolveNamedAssetNeeds(database, job.Needs)
		if err != nil {
			return err
		}
	}
	// state is non-nil here: queueProtocolCompatibilityError above refuses an
	// unpublished state before any capability is consulted, and the caller in
	// host_sync.go reaches this function through that same guard. The
	// unobserved-state case is reported there, not here.
	if len(artifactNeeds) > 0 && !state.Supports(opsqueue.CapabilityArtifactNeedV1) {
		return errors.New(opsqueue.MissingRunnerCapabilityBlockDetail(
			opsqueue.CapabilityArtifactNeedV1,
			"agent update required before dispatch",
		))
	}
	// The capability string predates producer artifacts, so a runner that
	// advertises it may still understand named assets only. Gate the newer
	// shape on the version rather than the string: without this, an agent that
	// has not been redeployed accepts a producer need it cannot satisfy and the
	// job waits on a marker nothing will write.
	if needsProducerArtifact(artifactNeeds) &&
		!state.SupportsArtifactNeedVersion(opsqueue.ArtifactNeedVersionProducerArtifacts) {
		return errors.New(opsqueue.MissingRunnerCapabilityBlockDetail(
			"artifact-need producer artifacts",
			"redeploy the agent on this host: weft deploy-agent",
		))
	}
	command := payloadGuardedCommand(job.Command, payloads)
	command = artifactNeedsGuardedCommand(command, artifactNeeds)
	entry := opsqueue.QueueEntry{
		JobID:                       job.ID,
		RunID:                       runID,
		WorkingDir:                  job.WorkingDir,
		Command:                     command,
		Description:                 job.Description,
		SourceSHA256:                sourceSHA256,
		SourceR2Key:                 sourceR2Key,
		SourceManifest:              sourceManifest,
		EnvVars:                     job.EnvVars,
		DepSpec:                     job.DepSpec,
		CPUAllotment:                job.CPUAllotment,
		GPU:                         job.GPU,
		GPUClass:                    job.GPUClass,
		GPUCount:                    job.RequestedGPUCount(),
		GPUMemGB:                    job.GPUMemGB,
		Interconnect:                job.RequestedInterconnect(),
		Platform:                    job.RequestedPlatform(),
		CPUCores:                    job.RequestedCPUCores(),
		CPUReserveCores:             job.RequestedCPUReserveCores(),
		RAMReservationKB:            jobRAMReservationKB(job),
		WallTimeSeconds:             int(job.WallTime() / time.Second),
		GPUIdleTimeoutSeconds:       job.GPUIdleTimeoutOverride(),
		StdoutSilenceTimeoutSeconds: job.StdoutSilenceTimeoutOverride(),
		Tags:                        job.Tags,
		OutputDirs:                  job.OutputDirs,
		Outputs:                     job.Outputs,
		Produces:                    job.Produces,
		Needs:                       job.Needs,
		ArtifactNeeds:               artifactNeeds,
		Payloads:                    payloads,
		SetupPolicy:                 job.SetupPolicy,
	}
	addCmd := opsqueue.NewAddCommand(entry)
	opts := opsqueue.AppendCommandOptions{Timeout: timeout}
	if err := appendQueueCommand(job.Host, addCmd, opts); err != nil {
		return err
	}
	recordQueueDispatchOK(database, job.ID)
	return nil
}

func queueProtocolCompatibilityError(host string, state *opsqueue.RunnerState) error {
	if state == nil {
		// Reaching here with a nil state (and no fetch error) means the
		// runner has never published state — it may not be started, or it
		// is running without publishing. `weft queue update` cannot fix
		// either (wb164).
		return fmt.Errorf(
			"queue runner on %s has not published state (required protocol=%d); start it with `weft queue start %s` — if it is already running, its state publication is failing (see `weft queue status %s`)",
			host, opsqueue.QueueProtocolVersion, host, host)
	}
	if state.QueueProtocolVersion < opsqueue.QueueProtocolVersion {
		return fmt.Errorf(
			"queue runner protocol version on %s is %d, older than required version %d; run `weft queue update %s`",
			host, state.QueueProtocolVersion, opsqueue.QueueProtocolVersion, host)
	}
	return nil
}

// recordHostAgentRuntimeObservation caches the identity read from the
// runner's state file. observedAt is when the caller read the state, never
// the state's own updated_at: that timestamp records the runner's last queue
// activity, so an idle runner's state can be hours old while its identity is
// freshly re-observed on every sync.
func recordHostAgentRuntimeObservation(database *sql.DB, host string, state *opsqueue.RunnerState, observedAt time.Time) {
	if database == nil || state == nil {
		return
	}
	// state.UpdatedAt is the runner's liveness heartbeat (saved at least
	// every few seconds), so prefer it: an SSH-transport state read carries
	// the runner's own stamp, and both writers of running_observed_at (this
	// recorder and the hostsync worker reading the same state file) then
	// agree on what the column means.
	if state.UpdatedAt > 0 {
		observedAt = time.Unix(state.UpdatedAt, 0)
	}
	if err := db.RecordHostAgentRuntime(
		database, host, state.AgentVersion, state.QueueProtocolVersion, observedAt); err != nil {
		slog.Warn("failed to record host agent runtime observation", "host", host, "error", err)
	}
}

// payloadGuardedCommand makes a payload-bearing queue entry fail closed on an
// older inventory agent that ignores the payloads JSON field. Current agents
// stage the files and set WEFT_PAYLOAD_DIR before invoking this command.
func payloadGuardedCommand(command string, payloads []opsqueue.Payload) string {
	if len(payloads) == 0 {
		return command
	}
	return `if [ -z "${WEFT_PAYLOAD_DIR:-}" ]; then echo "weft: payload staging unavailable; update the host agent" >&2; exit 78; fi; ` + command
}

// artifactNeedsGuardedCommand makes a structured-needs queue entry fail closed
// on an older inventory agent that ignores artifact_needs. Current agents stage
// the assets into the runtime working directory and set the guard variable.
func artifactNeedsGuardedCommand(command string, needs []opsqueue.ArtifactNeed) string {
	if len(needs) == 0 {
		return command
	}
	return `if [ "${WEFT_ARTIFACT_NEEDS_STAGED:-}" != 1 ]; then echo "weft: artifact staging unavailable; update the host agent" >&2; exit 78; fi; ` + command
}

func queuePayloadsForJob(database *sql.DB, jobID int64) ([]opsqueue.Payload, error) {
	rows, err := db.ListJobPayloads(database, jobID)
	if err != nil {
		return nil, err
	}
	payloads := make([]opsqueue.Payload, 0, len(rows))
	for _, payload := range rows {
		payloads = append(payloads, opsqueue.Payload{Name: payload.Name, SizeBytes: payload.SizeBytes, SHA256: payload.SHA256, R2Key: payload.R2Key})
	}
	return payloads, nil
}

func jobRAMReservationKB(job *db.Job) int64 {
	if job == nil {
		return 0
	}
	reservation := int64(job.RequestedCPUMemGB()) * 1024 * 1024
	if job.PlacementMeta != nil && job.PlacementMeta.PredictedRSSUpperKB != nil {
		predicted := int64(*job.PlacementMeta.PredictedRSSUpperKB)
		if predicted > reservation {
			reservation = predicted
		}
	}
	return reservation
}

// pinnedQueueSourceManifest converts durable submit-time metadata into the
// queue protocol without consulting the current working tree.
func pinnedQueueSourceManifest(job *db.Job) (*opsqueue.SourceManifest, bool, error) {
	if job == nil || job.Metadata == nil || job.Metadata.Source == nil || job.Metadata.Source.Pin == nil {
		return nil, false, nil
	}
	pin := job.Metadata.Source.Pin
	if strings.TrimSpace(pin.Hash) == "" {
		return nil, true, fmt.Errorf("job %d pinned source manifest has no hash", job.ID)
	}
	if len(pin.Roots) == 0 {
		return nil, true, fmt.Errorf("job %d pinned source manifest has no roots", job.ID)
	}
	manifest := &opsqueue.SourceManifest{SHA256: pin.Hash, Roots: make([]opsqueue.SourceRoot, 0, len(pin.Roots))}
	for i, root := range pin.Roots {
		if strings.TrimSpace(root.MountBasename) == "" {
			return nil, true, fmt.Errorf("job %d pinned source root %d has no mount basename", job.ID, i)
		}
		if strings.TrimSpace(root.Hash) == "" {
			return nil, true, fmt.Errorf("job %d pinned source root %d has no hash", job.ID, i)
		}
		if strings.TrimSpace(root.R2Key) == "" {
			return nil, true, fmt.Errorf("job %d pinned source root %d has no R2 key", job.ID, i)
		}
		manifest.Roots = append(manifest.Roots, opsqueue.SourceRoot{
			MountBasename: root.MountBasename,
			Hash:          root.Hash,
			R2Key:         root.R2Key,
			Blobs:         append([]dataplane.SourceBlob(nil), root.Blobs...),
		})
	}
	return manifest, true, nil
}

// QueueJobParams contains parameters for queueing a new job
type QueueJobParams struct {
	Host            string
	WorkingDir      string
	Command         string
	Description     string
	Project         string
	EnvVars         []string
	Tags            []string
	GPU             string // Explicit GPU setting; if empty, extracted from EnvVars
	GPUClass        string // GPU class name (e.g., "A100") — resolved to device at runtime
	GPUCount        int    // Exact GPU count requested on this host
	GPUMemGB        *int   // GPU memory reservation in GB per device
	Interconnect    string
	Platform        string
	CPUCores        int
	CPUReserveCores int    // absolute cores; normalized to a percent of the destination host at dispatch
	SetupPolicy     string // "" = auto-detect target setup, "none" = the job owns its environment
	GPUMemMaxGB     *int   // Legacy GPU memory upper metadata; ignored by placement
	DepSpec         string
	CPUAllotment    *int
	OutputDirs      []string // convention-based output directories from .weft.toml
	Inputs          []string // Data asset refs the job reads (e.g., "hf:meta-llama/Llama-3-8B")
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
	// Payloads are immutable input artifacts captured before admission. The
	// association rows are inserted in the same transaction as the job.
	Payloads []db.JobPayload
	// SubmitterSession is the opaque agent-session id of the submitter, used
	// to address the completion notification back to the session that asked
	// for the job. Only callers running in the submitting process may set it
	// (see config.Config.SubmitterSession); it is deliberately not read from
	// the environment here, because this function also runs inside long-lived
	// processes that may have inherited an unrelated session's environment.
	SubmitterSession string
	// EdgeProvenance is set only by the authenticated hub inbox poller.
	EdgeProvenance *db.EdgeSubmissionProvenance
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
	platform := params.Platform
	if platform == "" && params.CLIOverrides != nil {
		platform = params.CLIOverrides.Platform
	}
	if platform != "" {
		normalized, err := compat.NormalizePlatform(platform)
		if err != nil {
			return 0, err
		}
		overrides := db.CLIResourceOverrides{}
		if params.CLIOverrides != nil {
			overrides = *params.CLIOverrides
		}
		overrides.Platform = normalized
		params.CLIOverrides = &overrides
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
	if err := validateJobPayloads(params.Payloads); err != nil {
		return 0, err
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
			if err := requireMatchingPayloads(tx, jobID, params.Payloads); err != nil {
				return 0, err
			}
			return jobID, nil
		}
	}

	// Record job with queued status. Project, project_root, and
	// submitter_session go into the initial INSERT: the attempt created
	// immediately afterwards fires a lifecycle trigger that snapshots those
	// columns, so writing them later in this transaction would leave the
	// queued event with empty routing metadata.
	ident := db.SubmissionIdentity{Project: project, SubmitterSession: params.SubmitterSession, Edge: params.EdgeProvenance}
	var jobID int64
	if draft {
		if explicitID {
			return 0, fmt.Errorf("record draft job with explicit ID is not supported")
		}
		var err error
		jobID, err = db.RecordDraftJobWithIdentityTx(tx, params.Host, params.WorkingDir, params.Command, params.Description, gpu, ident)
		if err != nil {
			return 0, fmt.Errorf("record draft job: %w", err)
		}
	} else if explicitID {
		jobID = explicitJobID
		if err := db.RecordQueuedWithIdentityAndIDTx(tx, jobID, params.Host, params.WorkingDir, params.Command, params.Description, gpu, ident); err != nil {
			return 0, fmt.Errorf("record job: %w", err)
		}
	} else {
		var err error
		jobID, err = db.RecordQueuedWithIdentityTx(tx, params.Host, params.WorkingDir, params.Command, params.Description, gpu, ident)
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
	if params.CPUAllotment != nil {
		if err := db.SetJobCPUAllotment(tx, jobID, params.CPUAllotment); err != nil {
			return 0, fmt.Errorf("record CPU allotment: %w", err)
		}
	}
	if params.CPUReserveCores > 0 {
		reserve := params.CPUReserveCores
		if err := db.SetJobCPUReserveCores(tx, jobID, &reserve); err != nil {
			return 0, fmt.Errorf("record CPU reserve cores: %w", err)
		}
	}
	if params.SetupPolicy != "" {
		if err := db.SetJobSetupPolicy(tx, jobID, params.SetupPolicy); err != nil {
			return 0, fmt.Errorf("record setup policy: %w", err)
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
	for _, payload := range params.Payloads {
		payload.JobID = jobID
		if err := db.InsertJobPayload(tx, payload); err != nil {
			return 0, fmt.Errorf("record payload: %w", err)
		}
	}

	return jobID, nil
}

func validateJobPayloads(payloads []db.JobPayload) error {
	seen := make(map[string]struct{}, len(payloads))
	for _, payload := range payloads {
		if err := artifacts.ValidatePayloadName(payload.Name); err != nil {
			return err
		}
		if payload.StoredPath == "" || payload.SHA256 == "" || payload.R2Key == "" || payload.SizeBytes < 0 {
			return fmt.Errorf("payload %q has incomplete capture metadata", payload.Name)
		}
		if _, ok := seen[payload.Name]; ok {
			return fmt.Errorf("duplicate payload name %q", payload.Name)
		}
		seen[payload.Name] = struct{}{}
	}
	return nil
}

func requireMatchingPayloads(tx *sql.Tx, jobID int64, submitted []db.JobPayload) error {
	existing, err := db.ListJobPayloads(tx, jobID)
	if err != nil {
		return fmt.Errorf("read payloads for idempotent job %d: %w", jobID, err)
	}
	if len(existing) != len(submitted) {
		return fmt.Errorf("idempotency key already belongs to job %d with different payloads", jobID)
	}
	byName := make(map[string]db.JobPayload, len(existing))
	for _, payload := range existing {
		byName[payload.Name] = payload
	}
	for _, payload := range submitted {
		prior, ok := byName[payload.Name]
		if !ok || prior.SizeBytes != payload.SizeBytes || prior.SHA256 != payload.SHA256 {
			return fmt.Errorf("idempotency key already belongs to job %d with different payload %q", jobID, payload.Name)
		}
	}
	return nil
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

	if err := AppendJobToQueue(database, job, opts.Timeout); err != nil {
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
