package cmd

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/runner"
	"github.com/osteele/weft/internal/session"
	"github.com/osteele/weft/internal/workdir"
)

// startJobOptions controls how a job is started immediately on the remote host.
type startJobOptions struct {
	Host        string
	WorkingDir  string
	Command     string
	Description string
	EnvVars     []string
	Tags        []string
	GPUMemGB    *int
	Timeout     string
	OnPrepared  func(info StartJobPreparedInfo)
}

// StartJobPreparedInfo exposes metadata about the job once it has an ID.
type StartJobPreparedInfo struct {
	JobID       int64
	Host        string
	WorkingDir  string
	Command     string
	Description string
}

// startJobResult reports the outcome of the start operation.
type startJobResult struct {
	Info            StartJobPreparedInfo
	DeferredToQueue bool
}

func startJob(database *sql.DB, opts startJobOptions) (*startJobResult, error) {
	if opts.WorkingDir == "" {
		var err error
		opts.WorkingDir, err = session.DefaultWorkingDir()
		if err != nil {
			return nil, fmt.Errorf("get working dir: %w", err)
		}
	}

	// Always queue — the sync worker / queue runner handles actual execution.
	gpu := extractGPUFromEnvVars(opts.EnvVars)
	res, err := queueJob(database, queueJobOptions{
		Host:        opts.Host,
		WorkingDir:  opts.WorkingDir,
		Command:     opts.Command,
		Description: opts.Description,
		EnvVars:     opts.EnvVars,
		Tags:        opts.Tags,
		GPU:         gpu,
		GPUMemGB:    opts.GPUMemGB,
	})
	if err != nil {
		return nil, err
	}

	info := StartJobPreparedInfo{
		JobID:       res.JobID,
		Host:        opts.Host,
		WorkingDir:  opts.WorkingDir,
		Command:     opts.Command,
		Description: opts.Description,
	}

	if opts.OnPrepared != nil {
		opts.OnPrepared(info)
	}

	return &startJobResult{
		Info:            info,
		DeferredToQueue: true,
	}, nil
}

type queueJobResult struct {
	JobID    int64
	Deferred bool // Always false — kept for callers that check it
}

// queueJobOptions controls adding a job to a remote queue.
type queueJobOptions struct {
	Host             string
	WorkingDir       string
	Command          string
	Description      string
	Project          string
	EnvVars          []string
	Tags             []string
	GPU              string // Explicit GPU setting (extracted from EnvVars or set directly)
	GPUClass         string // GPU class name (e.g., "A100") — resolved to device at runtime
	GPUMemGB         *int   // GPU memory reservation in GB per device
	GPUMemStrict     bool   // Apply exact GPU memory floor when resolving from explicit GPUMemGB.
	GPUMemMaxGB      *int   // Legacy GPU memory upper metadata; ignored by placement
	CPUCores         int
	CPUMemGB         int
	Interconnect     string
	Dependencies     []queueDependency
	AutoStart        bool
	Inputs           []string // Data asset refs (e.g., "hf:meta-llama/Llama-3-8B")
	BestEffortInputs []string
	Outputs          []string // Data asset refs (e.g., "checkpoint:llama-ft-v1")
	OutputDirs       []string // Convention-based output directories from .weft.toml
	Produces         []string // Artifact specs this job produces
	Needs            []string // Artifact specs this job needs
	CloudAfter       []db.JobDependencyRef
	CloudNeeds       []string
	Disk             *db.JobDiskMetadata
	CLIOverrides     *db.CLIResourceOverrides
	MaxComputeCap    string
}

type queueDependency struct {
	JobID        int64
	AllowFailure bool
}

// resolveGPUMemGB returns the GPU memory reservation based on explicit flag, GPU, and GPU class.
// If gpuMemFlag > 0, use it. If GPU or GPU class is involved but no explicit flag, default to defaultGPUMemGB.
// Returns nil if no GPU involvement.
func resolveGPUMemGB(gpuMemFlag int, gpu string, gpuClass string) *int {
	if gpuMemFlag > 0 {
		return &gpuMemFlag
	}
	if gpu != "" || gpuClass != "" {
		defaultMem := defaultGPUMemGB
		return &defaultMem
	}
	return nil
}

// isNumericGPU returns true if the string looks like a GPU device index (digits and commas only).
func isNumericGPU(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c != ',' && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// buildCloudDependencyMetadata returns a JobMetadata carrying CloudAfter /
// CloudNeeds, or nil if both are empty. This helper is used by all submission
// paths so the metadata is persisted atomically as part of RecordQueuedJob —
// a separate SetJobMetadata call after RecordQueuedJob leaves a window in
// which the sync worker can dispatch the job before the metadata is written,
// silently skipping cloud artifact staging.
func buildCloudDependencyMetadata(cloudAfter []db.JobDependencyRef, cloudNeeds []string) *db.JobMetadata {
	if len(cloudAfter) == 0 && len(cloudNeeds) == 0 {
		return nil
	}
	meta := &db.JobMetadata{
		Dependencies: &db.JobDependencyMetadata{},
	}
	if len(cloudAfter) > 0 {
		meta.Dependencies.CloudAfter = append([]db.JobDependencyRef(nil), cloudAfter...)
	}
	if len(cloudNeeds) > 0 {
		meta.Dependencies.CloudNeeds = append([]string(nil), cloudNeeds...)
	}
	return meta
}

// extractGPUFromEnvVars finds and returns the CUDA_VISIBLE_DEVICES value from env vars
func extractGPUFromEnvVars(envVars []string) string {
	for _, ev := range envVars {
		if strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") {
			return strings.TrimPrefix(ev, "CUDA_VISIBLE_DEVICES=")
		}
	}
	return ""
}

// queueJob records a job locally via RecordQueuedJob (DB-only, no SSH).
// The caller is responsible for calling syncHostWithProgress to push to the remote.
func queueJob(database *sql.DB, opts queueJobOptions) (*queueJobResult, error) {
	gpu := opts.GPU
	if gpu == "" {
		gpu = extractGPUFromEnvVars(opts.EnvVars)
	}
	localDir := workdir.ResolveLocal(opts.WorkingDir)
	cpuCores := opts.CPUCores
	cpuMemGB := opts.CPUMemGB
	interconnect := strings.TrimSpace(opts.Interconnect)
	cliOverrides := opts.CLIOverrides
	scriptMeta, err := dataloc.ScanScriptMeta(localDir, opts.Command)
	if err != nil {
		return nil, err
	}
	if scriptMeta != nil {
		if cpuCores == 0 && scriptMeta.CPUCores > 0 {
			cpuCores = scriptMeta.CPUCores
			cliOverrides = ensureQueueCLIOverrides(cliOverrides)
			cores := scriptMeta.CPUCores
			cliOverrides.CPUCores = &cores
		}
		if cpuMemGB == 0 && scriptMeta.CPUMemGB > 0 {
			strict := scriptMeta.CPUMemStrict != nil && *scriptMeta.CPUMemStrict
			cpuMemGB = db.EffectiveCPUMemGB(scriptMeta.CPUMemGB, strict)
			cliOverrides = ensureQueueCLIOverrides(cliOverrides)
			mem := scriptMeta.CPUMemGB
			cliOverrides.CPUMemGB = &mem
			if strict {
				cliOverrides.CPUMemStrict = &strict
			}
		}
		if interconnect == "" && scriptMeta.Interconnect != "" {
			normalized, normalizeErr := normalizeInterconnect(scriptMeta.Interconnect)
			if normalizeErr != nil {
				return nil, normalizeErr
			}
			interconnect = normalized
			cliOverrides = ensureQueueCLIOverrides(cliOverrides)
			cliOverrides.Interconnect = interconnect
		}
	}
	cfg, _ := loadPredictorConfig()
	gpuMemGB, gpuMemMaxGB, _ := resolveEffectiveGPUMemAndCeiling(cfg, opts.GPUMemGB, gpu, opts.GPUClass, opts.GPUMemStrict, opts.Host, opts.Project, opts.Command, 0)
	if opts.GPUMemMaxGB != nil {
		gpuMemMaxGB = opts.GPUMemMaxGB
	}
	gpuMem := 0
	if gpuMemGB != nil {
		gpuMem = *gpuMemGB
	}
	resolvedConstraints, err := placement.ResolveConstraints(placement.ConstraintSource{
		GPUClass:     opts.GPUClass,
		GPUMemGB:     gpuMem,
		CPUCores:     cpuCores,
		CPUMemGB:     cpuMemGB,
		Interconnect: interconnect,
		Inputs:       opts.Inputs,
		Command:      opts.Command,
		Project:      opts.Project,
		Tags:         opts.Tags,
		LocalDir:     localDir,
		CLIOverrides: cliOverrides,
	})
	if err != nil {
		return nil, err
	}
	if err := validatePinnedHostQueueGate(opts.Host, resolvedConstraints.Constraints); err != nil {
		return nil, err
	}

	params := ops.QueueJobParams{
		Host:             opts.Host,
		WorkingDir:       opts.WorkingDir,
		Command:          opts.Command,
		Description:      opts.Description,
		Project:          opts.Project,
		EnvVars:          opts.EnvVars,
		Tags:             opts.Tags,
		GPU:              gpu,
		GPUClass:         opts.GPUClass,
		GPUMemGB:         gpuMemGB,
		GPUMemMaxGB:      gpuMemMaxGB,
		Interconnect:     interconnect,
		CPUCores:         cpuCores,
		DepSpec:          encodeQueueDependencies(opts.Dependencies),
		Inputs:           opts.Inputs,
		BestEffortInputs: opts.BestEffortInputs,
		Outputs:          opts.Outputs,
		OutputDirs:       opts.OutputDirs,
		Produces:         opts.Produces,
		Needs:            opts.Needs,
		Metadata:         buildCloudDependencyMetadata(opts.CloudAfter, opts.CloudNeeds),
		Disk:             opts.Disk,
		CLIOverrides:     cliOverrides,
		MaxComputeCap:    opts.MaxComputeCap,
	}

	jobID, err := ops.RecordQueuedJob(database, params)
	if err != nil {
		return nil, err
	}

	return &queueJobResult{
		JobID: jobID,
	}, nil
}

func ensureQueueCLIOverrides(overrides *db.CLIResourceOverrides) *db.CLIResourceOverrides {
	if overrides != nil {
		return overrides
	}
	return &db.CLIResourceOverrides{}
}

func applyEnvMap(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	vars := make([]string, 0, len(keys))
	for _, k := range keys {
		vars = append(vars, fmt.Sprintf("%s=%s", k, env[k]))
	}
	return vars
}

// mergeEnvVarsByKey merges two "KEY=value" slices with key-based dedup.
// Values from overlay replace values from base when the key matches.
// New keys from overlay are appended.
func mergeEnvVarsByKey(base, overlay []string) []string {
	if len(overlay) == 0 {
		return base
	}
	if len(base) == 0 {
		return overlay
	}
	result := make([]string, 0, len(base)+len(overlay))
	replaced := make(map[string]bool)
	for _, ov := range overlay {
		k, _, _ := strings.Cut(ov, "=")
		replaced[k] = true
	}
	for _, bv := range base {
		k, _, _ := strings.Cut(bv, "=")
		if !replaced[k] {
			result = append(result, bv)
		}
	}
	result = append(result, overlay...)
	return result
}

func encodeQueueDependencies(deps []queueDependency) string {
	if len(deps) == 0 {
		return ""
	}
	parts := make([]string, 0, len(deps))
	for _, dep := range deps {
		if dep.JobID <= 0 {
			continue
		}
		part := fmt.Sprintf("%d", dep.JobID)
		if dep.AllowFailure {
			part += ":any"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ",")
}

func ensureSameHostDependency(database *sql.DB, depID int64, host string) error {
	if depID <= 0 {
		return fmt.Errorf("invalid dependency job ID %d", depID)
	}
	job, err := db.GetJobByID(database, depID)
	if err != nil {
		return fmt.Errorf("lookup dependency job %s: %w", ids.FormatJobID(depID), err)
	}
	if job == nil {
		return fmt.Errorf("dependency job %s not found", ids.FormatJobID(depID))
	}
	if job.IsRentalJob() || strings.TrimSpace(job.Host) == "" {
		return nil
	}
	if job.Host != host {
		return fmt.Errorf("dependency job %s runs on host %s, target on %s: %w", ids.FormatJobID(depID), job.Host, host, errCrossHostDep)
	}
	return nil
}

func resolveDependencyForTarget(database *sql.DB, depID int64, host string, allowFailure bool) (*queueDependency, *db.JobDependencyRef, error) {
	if depID <= 0 {
		return nil, nil, fmt.Errorf("invalid dependency job ID %d", depID)
	}
	job, err := db.GetJobByID(database, depID)
	if err != nil {
		return nil, nil, fmt.Errorf("lookup dependency job %s: %w", ids.FormatJobID(depID), err)
	}
	if job == nil {
		return nil, nil, fmt.Errorf("dependency job %s not found", ids.FormatJobID(depID))
	}
	if job.IsRentalJob() || strings.TrimSpace(job.Host) == "" {
		ref := &db.JobDependencyRef{JobID: depID, AllowFailure: allowFailure}
		return nil, ref, nil
	}
	if strings.TrimSpace(host) != "" && job.Host != host {
		return nil, nil, fmt.Errorf("dependency job %s runs on host %s, target on %s: %w", ids.FormatJobID(depID), job.Host, host, errCrossHostDep)
	}
	return &queueDependency{JobID: depID, AllowFailure: allowFailure}, nil, nil
}

// resolveArtifactNeedsPlacement validates `--needs` specs and applies on-prem
// host pinning. All specs are returned in a single slice, stored on jobs.needs
// and later re-classified at placement/launch time (see
// internal/campaign/needs_classify.go) into same-instance co-location
// (CloudAfter) or cross-instance R2 staging (CloudNeeds).
func resolveArtifactNeedsPlacement(database *sql.DB, needs []string, host string) (string, []string, error) {
	resolvedHost := strings.TrimSpace(host)
	seenJobs := make(map[int64]bool)
	out := make([]string, 0, len(needs))

	for _, spec := range needs {
		parsed, err := runner.ParseNeedsSpec(spec)
		if err != nil {
			return "", nil, err
		}

		// Named assets are R2-staged at launch time and do not pin placement
		// to any specific host. Skip the producer-job resolution path.
		if parsed.IsAsset() {
			out = append(out, spec)
			continue
		}

		job, err := db.GetJobByID(database, parsed.Version)
		if err != nil {
			return "", nil, fmt.Errorf("lookup artifact producer job %s: %w", ids.FormatJobID(parsed.Version), err)
		}
		if job == nil {
			return "", nil, fmt.Errorf("artifact producer job %s not found", ids.FormatJobID(parsed.Version))
		}

		if err := validateNeedsPath(spec, parsed.Path, job, database); err != nil {
			return "", nil, err
		}

		// On-prem producer: pin the consumer to the producer's host so the
		// shared filesystem / queue-runner dep chain handles artifact reuse.
		// Rental and unplaced producers are classified later at launch time.
		if !job.IsRentalJob() && strings.TrimSpace(job.Host) != "" {
			producerHost := strings.TrimSpace(job.Host)
			if !seenJobs[parsed.Version] {
				if resolvedHost == "" {
					resolvedHost = producerHost
				} else if producerHost != resolvedHost {
					return "", nil, fmt.Errorf("artifact dependency %q is on host %s, but target host is %s", spec, producerHost, resolvedHost)
				}
				seenJobs[parsed.Version] = true
			}
		}
		out = append(out, spec)
	}

	return resolvedHost, out, nil
}

// validateNeedsPath rejects a --needs spec whose path the producer job could
// not plausibly have produced, catching consumer path typos at submit time
// instead of letting them surface as missing-file errors on a rental instance.
//
// It uses the best information available about the producer:
//   - If the producer has completed, the path must match one of its actually
//     recorded output artifacts.
//   - Otherwise the path must match a declared --produces path or fall under
//     one of the producer's conventional output directories (output/, outputs/,
//     or custom dirs from .weft.toml).
//
// A producer that declared neither --produces nor any output directory (legacy
// jobs predating the output-dirs column) accepts any path.
func validateNeedsPath(spec, needsPath string, producer *db.Job, database *sql.DB) error {
	want := normalizeNeedsPath(needsPath)

	if producer.Status == db.StatusCompleted {
		arts, err := db.ListArtifactsByJob(database, producer.ID)
		if err != nil {
			return fmt.Errorf("look up recorded outputs for producer job %s: %w", ids.FormatJobID(producer.ID), err)
		}
		if len(arts) > 0 {
			recorded := make([]string, 0, len(arts))
			for _, a := range arts {
				got := normalizeNeedsPath(a.Path)
				if got == want {
					return nil
				}
				if got != "" {
					recorded = append(recorded, got)
				}
			}
			return fmt.Errorf(
				"--needs %q: producer job %s completed and recorded outputs %v, which do not include %q (typo? correct the --needs path)",
				spec, ids.FormatJobID(producer.ID), recorded, needsPath,
			)
		}
		// Completed but no artifacts recorded locally yet (sync lag): fall
		// through to the declared/conventional check rather than reject.
	}

	if needsPathDeclaredOrConventional(producer, want) {
		return nil
	}
	return fmt.Errorf(
		"--needs %q: producer job %s declares --produces %v and writes outputs to %v, none of which cover %q (typo? add the path to --produces, or correct the --needs path)",
		spec, ids.FormatJobID(producer.ID), producer.Produces, producer.OutputDirs, needsPath,
	)
}

// needsPathDeclaredOrConventional reports whether want (a normalized path)
// matches a declared --produces path or falls under a conventional output
// directory of the producer. A producer with neither --produces nor output
// directories matches any path.
func needsPathDeclaredOrConventional(producer *db.Job, want string) bool {
	if len(producer.Produces) == 0 && len(producer.OutputDirs) == 0 {
		return true
	}
	for _, raw := range producer.Produces {
		if got := normalizeNeedsPath(runner.ParseProducesSpec(raw).Path); got != "" && got == want {
			return true
		}
	}
	for _, dir := range producer.OutputDirs {
		d := normalizeNeedsPath(dir)
		if d == "" {
			continue
		}
		if !strings.HasSuffix(d, "/") {
			d += "/"
		}
		if strings.HasPrefix(want, d) {
			return true
		}
	}
	return false
}

// normalizeNeedsPath trims surrounding whitespace and a leading slash so
// artifact paths compare consistently regardless of how they were written.
func normalizeNeedsPath(p string) string {
	return strings.TrimPrefix(strings.TrimSpace(p), "/")
}
