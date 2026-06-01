package cmd

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/workdir"
	"github.com/spf13/cobra"
)

var restartCmd = &cobra.Command{
	Use:     "restart [job-id]...",
	Aliases: []string{"retry"},
	Short:   "Restart a killed, dead, failed, canceled, or completed job",
	Long: `Restart a job by requeuing it with the same ID.

The previous run is archived and the job is reset to queued status.

Examples:
  weft restart 42
  weft retry 42
  weft restart 42 43 44
  weft retry --unplaced
  weft retry 42 --from-scratch
  weft retry 42 --checkpointed`,
	Args: usageArgs(cobra.ArbitraryArgs),
	RunE: runRestart,
}

var (
	restartGPU           string
	restartGPUClass      string
	restartProvider      string
	restartGPUMem        int
	restartDiskGB        int
	restartRuntimeDiskGB int
	restartGPUMemStrict  bool
	restartUnplaced      bool
	restartFromScratch   bool
	restartCheckpointed  bool
)

type restartOverrides struct {
	GPU                 string
	GPUClass            string
	Provider            *string
	GPUMemGB            *int
	GPUMemStrict        bool
	GPUMemHardwareFloor bool
	HasAny              bool
	HasGPUMem           bool
	HasGPUMemStrict     bool
	HasProvider         bool
	HasDisk             bool
	DiskGB              *int
	HasRuntimeDisk      bool
	RuntimeDiskGB       *int
}

func init() {
	rootCmd.AddCommand(restartCmd)
	addRestartFlags(restartCmd)
}

func runRestart(cmd *cobra.Command, args []string) error {
	overrides, err := parseRestartOverrides(cmd)
	if err != nil {
		return err
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	jobIDs, err := resolveRestartTargetJobIDs(database, args)
	if err != nil {
		return err
	}

	// Sync non-terminal jobs before restarting to get latest cloud status
	var jobsToSync []*db.Job
	for _, jobID := range jobIDs {
		job, err := db.GetJobByID(database, jobID)
		if err != nil || job == nil {
			continue
		}
		jobsToSync = append(jobsToSync, job)
	}
	if len(jobsToSync) > 0 {
		quickSyncJobs(database, jobsToSync, FastSyncTimeout, FastCloudSyncTimeout)
	}

	var errors []string
	for _, jobID := range jobIDs {
		if err := restartJob(database, jobID, overrides); err != nil {
			errors = append(errors, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
		}
	}

	if len(errors) > 0 {
		for _, e := range errors {
			fmt.Fprintln(os.Stderr, e)
		}
		return fmt.Errorf("%d job(s) could not be restarted", len(errors))
	}
	return nil
}

func addRestartFlags(command *cobra.Command) {
	command.Flags().StringVar(&restartGPU, "gpu", "", "GPU constraint override: device index, class, or class>=NGB (e.g., 1, a100, nvidia>=24GB)")
	command.Flags().StringVar(&restartGPUClass, "gpu-class", "", "GPU class or generation override (e.g., a100, ampere, ampere+); '+' means that generation or newer")
	command.Flags().StringVar(&restartProvider, "provider", "", "Cloud provider override for rental placement (vastai or runpod)")
	command.Flags().IntVar(&restartGPUMem, "gpu-mem", 0, "GPU memory reservation override in GB per device (0 clears)")
	command.Flags().IntVar(&restartDiskGB, "disk", 0, "Rental instance disk floor override in GB (0 clears)")
	command.Flags().IntVar(&restartRuntimeDiskGB, "runtime-disk", 0, "Extra rental scratch/cache disk headroom override in GB (0 clears)")
	command.Flags().BoolVar(&restartGPUMemStrict, "gpu-mem-strict", false, "Use exact gpu-mem matching without default safety headroom")
	command.Flags().BoolVar(&restartUnplaced, "unplaced", false, "Retry all queued unplaced jobs")
	command.Flags().BoolVar(&restartFromScratch, "from-scratch", false, "Force a fresh attempt and ignore checkpoint/resume assumptions")
	command.Flags().BoolVar(&restartCheckpointed, "checkpointed", false, "Retry expecting the command to resume from existing checkpoints")
	command.MarkFlagsMutuallyExclusive("from-scratch", "checkpointed")
}

func resolveRestartTargetJobIDs(database *sql.DB, args []string) ([]int64, error) {
	if restartUnplaced {
		if len(args) > 0 {
			return nil, usageErrorf("cannot combine job IDs with --unplaced")
		}
		jobs, err := db.ListUnplacedJobs(database)
		if err != nil {
			return nil, fmt.Errorf("list unplaced jobs: %w", err)
		}
		jobIDs := make([]int64, 0, len(jobs))
		for _, job := range jobs {
			if job != nil && job.EffectiveStatus() == db.StatusQueued {
				jobIDs = append(jobIDs, job.ID)
			}
		}
		if len(jobIDs) == 0 {
			return nil, fmt.Errorf("no unplaced queued jobs to retry")
		}
		slices.Sort(jobIDs)
		return jobIDs, nil
	}
	if len(args) == 0 {
		return nil, usageErrorf("requires at least one job ID, or use --unplaced")
	}
	return ParseJobIDs(args)
}

func parseRestartOverrides(cmd *cobra.Command) (restartOverrides, error) {
	var out restartOverrides
	if restartFromScratch && restartCheckpointed {
		return out, fmt.Errorf("--from-scratch and --checkpointed are mutually exclusive")
	}
	gpuValue := restartGPU
	gpuClassValue := restartGPUClass
	hasGPUMem := cmd.Flags().Changed("gpu-mem")
	hasGPUMemStrict := cmd.Flags().Changed("gpu-mem-strict")
	hasProvider := cmd.Flags().Changed("provider")
	hasDisk := cmd.Flags().Changed("disk")
	hasRuntimeDisk := cmd.Flags().Changed("runtime-disk")
	out.HasGPUMem = hasGPUMem
	out.HasGPUMemStrict = hasGPUMemStrict
	out.GPUMemStrict = restartGPUMemStrict
	out.HasProvider = hasProvider
	out.HasDisk = hasDisk
	out.HasRuntimeDisk = hasRuntimeDisk

	if gpuValue != "" && gpuClassValue != "" {
		return out, fmt.Errorf("--gpu and --gpu-class cannot be used together")
	}
	if gpuValue != "" && !isNumericGPU(gpuValue) {
		parsedClass, parsedMem, err := parseGPUFlag(gpuValue)
		if err != nil {
			return out, fmt.Errorf("--gpu: %w", err)
		}
		gpuClassValue = parsedClass
		if parsedMem > 0 {
			if hasGPUMem {
				return out, fmt.Errorf("--gpu with >=NGB and --gpu-mem cannot be used together")
			}
			restartGPUMem = parsedMem
			hasGPUMem = true
			out.HasGPUMem = true
			out.GPUMemHardwareFloor = true
		}
		gpuValue = ""
	}

	if hasGPUMem {
		mem := restartGPUMem
		if mem > 0 {
			effective := applyGPUMemHeadroom(mem, true, out.GPUMemStrict, out.GPUMemHardwareFloor)
			out.GPUMemGB = &effective
		} else {
			out.GPUMemGB = nil
		}
	}
	if hasProvider {
		normalizedProvider, providerErr := normalizeProviderFlag(restartProvider)
		if providerErr != nil {
			return out, fmt.Errorf("--provider: %w", providerErr)
		}
		out.Provider = &normalizedProvider
	}
	if hasDisk {
		disk := restartDiskGB
		out.DiskGB = &disk
	}
	if hasRuntimeDisk {
		runtimeDisk := restartRuntimeDiskGB
		out.RuntimeDiskGB = &runtimeDisk
	}
	out.GPU = gpuValue
	out.GPUClass = gpuClassValue
	out.HasAny = out.GPU != "" || out.GPUClass != "" || hasGPUMem || hasGPUMemStrict || hasProvider || hasDisk || hasRuntimeDisk
	return out, nil
}

func applyRestartOverrides(database *sql.DB, job *db.Job, overrides restartOverrides) ([]string, error) {
	var strictOverride *bool
	if overrides.HasGPUMemStrict {
		strictOverride = &overrides.GPUMemStrict
	}
	updates, err := applyScriptGPUDefaults(database, job, strictOverride)
	if err != nil {
		return nil, err
	}
	diskUpdates, err := applyScriptDiskDefaults(database, job)
	if err != nil {
		return nil, err
	}
	updates = append(updates, diskUpdates...)

	if !overrides.HasAny {
		return updates, nil
	}
	if overrides.GPU != "" {
		if err := db.SetJobGPU(database, job.ID, overrides.GPU); err != nil {
			return nil, fmt.Errorf("update gpu: %w", err)
		}
		job.GPU = overrides.GPU
		if job.GPUClass != "" {
			if err := db.SetJobGPUClass(database, job.ID, ""); err != nil {
				return nil, fmt.Errorf("clear gpu-class: %w", err)
			}
			job.GPUClass = ""
		}
		if err := setJobCLIGPUOverride(database, job, overrides.GPU); err != nil {
			return nil, fmt.Errorf("update gpu override: %w", err)
		}
		updates = append(updates, fmt.Sprintf("gpu: %s", overrides.GPU))
	}
	if overrides.GPUClass != "" {
		if err := db.SetJobGPUClass(database, job.ID, overrides.GPUClass); err != nil {
			return nil, fmt.Errorf("update gpu-class: %w", err)
		}
		if job.GPU != "" {
			if err := db.SetJobGPU(database, job.ID, ""); err != nil {
				return nil, fmt.Errorf("clear gpu: %w", err)
			}
			job.GPU = ""
		}
		job.GPUClass = overrides.GPUClass
		if err := setJobCLIGPUClassOverride(database, job, overrides.GPUClass); err != nil {
			return nil, fmt.Errorf("update gpu-class override: %w", err)
		}
		updates = append(updates, fmt.Sprintf("gpu-class: %s", overrides.GPUClass))
	}
	if overrides.HasGPUMem {
		if err := db.SetJobGPUMemGB(database, job.ID, overrides.GPUMemGB); err != nil {
			return nil, fmt.Errorf("update gpu-mem: %w", err)
		}
		job.GPUMemGB = overrides.GPUMemGB
		if err := setJobCLIGPUMemOverride(database, job, overrides.GPUMemGB); err != nil {
			return nil, fmt.Errorf("update gpu-mem override: %w", err)
		}
		if overrides.GPUMemGB == nil {
			updates = append(updates, "gpu-mem: cleared")
		} else {
			updates = append(updates, fmt.Sprintf("gpu-mem: %d GB", *overrides.GPUMemGB))
		}
	}
	if overrides.HasGPUMemStrict && !overrides.HasGPUMem {
		if err := setJobCLIGPUMemStrictOverride(database, job, overrides.GPUMemStrict); err != nil {
			return nil, fmt.Errorf("update gpu-mem-strict override: %w", err)
		}
	}
	if overrides.HasProvider {
		normalizedProvider := ""
		if overrides.Provider != nil {
			normalizedProvider = *overrides.Provider
		}
		if normalizedProvider != "" && job.HasInventoryHost() {
			return nil, fmt.Errorf("--provider=%s cannot be set while job is queued on inventory host %q; unplace the job first", normalizedProvider, job.Host)
		}
		newTags, providerErr := withProviderTag(job.Tags, normalizedProvider)
		if providerErr != nil {
			return nil, fmt.Errorf("--provider: %w", providerErr)
		}
		if err := db.SetJobTags(database, job.ID, newTags); err != nil {
			return nil, fmt.Errorf("update provider tag: %w", err)
		}
		job.Tags = newTags
		if normalizedProvider == "" {
			updates = append(updates, "provider: cleared")
		} else {
			updates = append(updates, fmt.Sprintf("provider: %s", normalizedProvider))
		}
	}
	if overrides.HasDisk || overrides.HasRuntimeDisk {
		diskGB, runtimeDiskGB := currentJobDisk(job)
		if overrides.HasDisk {
			diskGB = *overrides.DiskGB
			if err := setJobCLIDiskOverride(database, job, overrides.DiskGB); err != nil {
				return nil, fmt.Errorf("update disk override: %w", err)
			}
			if diskGB == 0 {
				updates = append(updates, "disk: cleared")
			} else {
				updates = append(updates, fmt.Sprintf("disk: %d GB", diskGB))
			}
		}
		if overrides.HasRuntimeDisk {
			runtimeDiskGB = *overrides.RuntimeDiskGB
			if err := setJobCLIRuntimeDiskOverride(database, job, overrides.RuntimeDiskGB); err != nil {
				return nil, fmt.Errorf("update runtime-disk override: %w", err)
			}
			if runtimeDiskGB == 0 {
				updates = append(updates, "runtime-disk: cleared")
			} else {
				updates = append(updates, fmt.Sprintf("runtime-disk: %d GB", runtimeDiskGB))
			}
		}
		if err := setJobDiskMetadata(database, job, buildDiskMetadata(diskGB, runtimeDiskGB)); err != nil {
			return nil, fmt.Errorf("update disk metadata: %w", err)
		}
	}
	return updates, nil
}

func applyScriptGPUDefaults(database *sql.DB, job *db.Job, strictOverride *bool) ([]string, error) {
	localDir := workdir.ResolveLocal(job.WorkingDir)
	meta, err := dataloc.ScanScriptMeta(localDir, job.Command)
	if err != nil {
		slog.Warn("script metadata error", "error", err)
		return nil, nil
	}
	// When there's no script PEP 723 [tool.weft] block, leave the job's
	// resource fields alone. The script provides no new information to merge,
	// and the existing values may have come from CLI or from a path we can't
	// infer.
	if meta == nil {
		return nil, nil
	}

	metaGPU, metaGPUClass, metaGPUMem, metaMemHardware, err := expandGPUFlag(meta.GPU, meta.GPUClass, meta.GPUMemGB)
	if err != nil {
		return nil, fmt.Errorf("parse script metadata gpu: %w", err)
	}
	metaStrict := false
	metaStrictSet := false
	if meta.GPUMemStrict != nil {
		metaStrict = *meta.GPUMemStrict
		metaStrictSet = true
	}

	cliGPU, cliGPUClass, cliGPUMem := "", "", 0
	cliMemHardware := false
	cliGPUMemSet, cliStrict, cliStrictSet := false, false, false
	if o := job.CLIResourceOverrides; o != nil {
		cliGPU, cliGPUClass = o.GPU, o.GPUClass
		if o.GPUMemGB != nil {
			cliGPUMem = *o.GPUMemGB
			cliGPUMemSet = true
		}
		if o.GPUMemStrict != nil {
			cliStrict = *o.GPUMemStrict
			cliStrictSet = true
		}
		var err error
		cliGPU, cliGPUClass, cliGPUMem, cliMemHardware, err = expandGPUFlag(cliGPU, cliGPUClass, cliGPUMem)
		if err != nil {
			return nil, fmt.Errorf("parse cli gpu override: %w", err)
		}
		if cliGPUMem > 0 {
			cliGPUMemSet = true
		}
	}

	// Precedence: strictOverride (retry-time flag) > CLI intent > script meta.
	effGPU := cliGPU
	if effGPU == "" {
		effGPU = metaGPU
	}
	effGPUClass := cliGPUClass
	// Only fall back to script gpu-class when no CLI intent exists for gpu
	// OR gpu-class — matches cmd/run.go:316-323 submission precedence.
	if effGPUClass == "" && effGPU == "" {
		effGPUClass = metaGPUClass
	}
	effStrict := false
	switch {
	case strictOverride != nil:
		effStrict = *strictOverride
	case cliStrictSet:
		effStrict = cliStrict
	case metaStrictSet:
		effStrict = metaStrict
	}
	effGPUMemRaw := 0
	effMemHardware := false
	if cliGPUMemSet {
		effGPUMemRaw = cliGPUMem
		effMemHardware = cliMemHardware
	} else if metaGPUMem > 0 {
		effGPUMemRaw = metaGPUMem
		effMemHardware = metaMemHardware
	}

	var updates []string

	if job.GPU != effGPU {
		if err := db.SetJobGPU(database, job.ID, effGPU); err != nil {
			return nil, fmt.Errorf("update gpu: %w", err)
		}
		job.GPU = effGPU
		if effGPU == "" {
			updates = append(updates, "gpu: cleared")
		} else {
			updates = append(updates, fmt.Sprintf("gpu: %s", effGPU))
		}
	}
	if !strings.EqualFold(job.GPUClass, effGPUClass) {
		if err := db.SetJobGPUClass(database, job.ID, effGPUClass); err != nil {
			return nil, fmt.Errorf("update gpu-class: %w", err)
		}
		job.GPUClass = effGPUClass
		if effGPUClass == "" {
			updates = append(updates, "gpu-class: cleared")
		} else {
			updates = append(updates, fmt.Sprintf("gpu-class: %s", effGPUClass))
		}
	}

	var effMem *int
	if effGPUMemRaw > 0 {
		m := applyGPUMemHeadroom(effGPUMemRaw, true, effStrict, effMemHardware)
		effMem = &m
	}
	memChanged := (effMem == nil) != (job.GPUMemGB == nil) ||
		(effMem != nil && job.GPUMemGB != nil && *effMem != *job.GPUMemGB)
	if memChanged {
		if err := db.SetJobGPUMemGB(database, job.ID, effMem); err != nil {
			return nil, fmt.Errorf("update gpu-mem: %w", err)
		}
		job.GPUMemGB = effMem
		switch {
		case effMem == nil:
			updates = append(updates, "gpu-mem: cleared")
		case effMemHardware:
			updates = append(updates, fmt.Sprintf("gpu-mem: %d GB (hardware floor)", *effMem))
		case effStrict:
			updates = append(updates, fmt.Sprintf("gpu-mem: %d GB (strict)", *effMem))
		default:
			updates = append(updates, fmt.Sprintf("gpu-mem: %d GB (+%dGB headroom)", *effMem, defaultGPUMemHeadroomGB))
		}
	}

	if len(meta.UvArgs) > 0 {
		rewritten := dataloc.ApplyUvArgs(job.Command, meta.UvArgs)
		if rewritten != job.Command {
			if err := db.SetJobCommand(database, job.ID, rewritten); err != nil {
				return nil, fmt.Errorf("update command from script metadata uv-args: %w", err)
			}
			job.Command = rewritten
			updates = append(updates, fmt.Sprintf("command: %s (from script metadata uv-args)", rewritten))
		}
	}
	if meta.PreInstall != "" && !strings.HasPrefix(job.Command, meta.PreInstall) {
		rewritten := meta.PreInstall + " && " + job.Command
		if err := db.SetJobCommand(database, job.ID, rewritten); err != nil {
			return nil, fmt.Errorf("update command from script metadata pre-install: %w", err)
		}
		job.Command = rewritten
		updates = append(updates, fmt.Sprintf("command: %s (from script metadata pre-install)", rewritten))
	}
	if len(meta.Env) > 0 {
		merged := mergeEnvVarsByKey(job.EnvVars, applyEnvMap(meta.Env))
		if !slices.Equal(merged, job.EnvVars) {
			if err := db.SetJobEnvVars(database, job.ID, merged); err != nil {
				return nil, fmt.Errorf("update env vars from script metadata: %w", err)
			}
			job.EnvVars = merged
			updates = append(updates, fmt.Sprintf("env: %v (from script metadata)", meta.Env))
		}
	}
	return updates, nil
}

func applyScriptDiskDefaults(database *sql.DB, job *db.Job) ([]string, error) {
	localDir := workdir.ResolveLocal(job.WorkingDir)
	meta, err := dataloc.ScanScriptMeta(localDir, job.Command)
	if err != nil {
		slog.Warn("script metadata error", "error", err)
		return nil, nil
	}

	diskGB := 0
	runtimeDiskGB := 0
	diskSet := false
	runtimeDiskSet := false
	if o := job.CLIResourceOverrides; o != nil {
		if o.DiskGB != nil {
			diskGB = *o.DiskGB
			diskSet = true
		}
		if o.RuntimeDiskGB != nil {
			runtimeDiskGB = *o.RuntimeDiskGB
			runtimeDiskSet = true
		}
	}
	if !diskSet && meta != nil && meta.DiskGB > 0 {
		diskGB = meta.DiskGB
		diskSet = true
	}
	if !runtimeDiskSet && meta != nil && meta.RuntimeDiskGB > 0 {
		runtimeDiskGB = meta.RuntimeDiskGB
		runtimeDiskSet = true
	}

	newDisk := buildDiskMetadata(diskGB, runtimeDiskGB)
	if sameDiskMetadata(jobDiskMetadata(job), newDisk) {
		return nil, nil
	}
	if err := setJobDiskMetadata(database, job, newDisk); err != nil {
		return nil, fmt.Errorf("update disk metadata from script metadata: %w", err)
	}
	var updates []string
	switch {
	case newDisk == nil:
		updates = append(updates, "disk metadata: cleared")
	default:
		if diskSet && diskGB > 0 {
			updates = append(updates, fmt.Sprintf("disk: %d GB", diskGB))
		}
		if runtimeDiskSet && runtimeDiskGB > 0 {
			updates = append(updates, fmt.Sprintf("runtime-disk: %d GB", runtimeDiskGB))
		}
	}
	return updates, nil
}

func currentJobDisk(job *db.Job) (int, int) {
	disk := jobDiskMetadata(job)
	if disk == nil {
		return 0, 0
	}
	return disk.DiskGB, disk.RuntimeDiskGB
}

func jobDiskMetadata(job *db.Job) *db.JobDiskMetadata {
	if job == nil || job.Metadata == nil {
		return nil
	}
	return job.Metadata.Disk
}

func sameDiskMetadata(a, b *db.JobDiskMetadata) bool {
	if a == nil || (a.DiskGB <= 0 && a.RuntimeDiskGB <= 0) {
		a = nil
	}
	if b == nil || (b.DiskGB <= 0 && b.RuntimeDiskGB <= 0) {
		b = nil
	}
	if a == nil || b == nil {
		return a == b
	}
	return a.DiskGB == b.DiskGB && a.RuntimeDiskGB == b.RuntimeDiskGB
}

func setJobDiskMetadata(database *sql.DB, job *db.Job, disk *db.JobDiskMetadata) error {
	meta := cloneJobMetadata(job.Metadata)
	if meta == nil {
		meta = &db.JobMetadata{}
	}
	meta.Disk = disk
	meta = persistentOrNonEmptyJobMetadata(meta)
	if err := db.SetJobMetadata(database, job.ID, meta); err != nil {
		return err
	}
	job.Metadata = meta
	return nil
}

func cloneJobMetadata(source *db.JobMetadata) *db.JobMetadata {
	if source == nil {
		return nil
	}
	clone := *source
	if source.Dependencies != nil {
		deps := *source.Dependencies
		deps.CloudAfter = append([]db.JobDependencyRef(nil), source.Dependencies.CloudAfter...)
		deps.CloudNeeds = append([]string(nil), source.Dependencies.CloudNeeds...)
		clone.Dependencies = &deps
	}
	if source.Disk != nil {
		disk := *source.Disk
		clone.Disk = &disk
	}
	return &clone
}

func persistentAttemptMetadata(meta *db.JobMetadata) *db.JobMetadata {
	if meta == nil {
		return nil
	}
	out := &db.JobMetadata{}
	if meta.Dependencies != nil {
		deps := &db.JobDependencyMetadata{}
		deps.CloudAfter = append([]db.JobDependencyRef(nil), meta.Dependencies.CloudAfter...)
		deps.CloudNeeds = append([]string(nil), meta.Dependencies.CloudNeeds...)
		if len(deps.CloudAfter) > 0 || len(deps.CloudNeeds) > 0 {
			out.Dependencies = deps
		}
	}
	if meta.Disk != nil && (meta.Disk.DiskGB > 0 || meta.Disk.RuntimeDiskGB > 0) {
		disk := *meta.Disk
		out.Disk = &disk
	}
	return persistentOrNonEmptyJobMetadata(out)
}

func persistentOrNonEmptyJobMetadata(meta *db.JobMetadata) *db.JobMetadata {
	if meta == nil {
		return nil
	}
	if meta.CPU == nil && meta.Resource == nil && meta.Telemetry == nil && meta.Dependencies == nil && meta.Disk == nil {
		return nil
	}
	return meta
}

func restartJob(database *sql.DB, jobID int64, overrides restartOverrides) error {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return db.ErrJobNotFound
	}

	// Validate job can be retried
	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus == db.StatusQueued {
		updates, err := applyRestartOverrides(database, job, overrides)
		if err != nil {
			return err
		}
		if err := validatePinnedHostQueueGate(job.Host, job.GPUClass, job.GPUMemGB); err != nil {
			return err
		}
		if err := ops.RefreshProjectDerivedMetadata(database, job.ID, job.WorkingDir, job.Command, job.Inputs); err != nil {
			return err
		}
		if len(updates) > 0 {
			if err := db.SetJobPlacementReasons(database, job.ID, nil); err != nil {
				return fmt.Errorf("clear stale placement reasons: %w", err)
			}
			job.PlacementReasons = nil
		}
		queuedEnded := job.EndTime != nil && *job.EndTime > 0
		cloudAttemptCount, err := db.CountLaunchAttempts(database, jobID)
		if err != nil {
			return fmt.Errorf("count cloud attempts: %w", err)
		}
		hasCloudRetryHistory := cloudAttemptCount > 0
		shouldForceFreshAttempt := queuedEnded || hasCloudRetryHistory || restartFromScratch
		if !shouldForceFreshAttempt && len(updates) == 0 {
			fmt.Printf("Job %s is already queued; metadata refreshed, changed sources will be re-synced on dispatch\n", ids.FormatJobID(jobID))
			tryResumeRunawayBreaker(database, job)
			return nil
		}

		if shouldForceFreshAttempt {
			// Remove processed tag so the retried job appears in unprocessed listings.
			if job.HasTag(db.ProcessedTag) {
				if err := db.RemoveJobTag(database, jobID, db.ProcessedTag); err != nil {
					return fmt.Errorf("remove processed tag: %w", err)
				}
			}

			retryHost := ""
			if job.HasInventoryHost() {
				retryHost = job.Host
			}
			if err := db.RequeueFreshAttemptByID(database, jobID, retryHost); err != nil {
				return fmt.Errorf("create fresh queued attempt: %w", err)
			}
			if err := db.SetJobMetadata(database, jobID, persistentAttemptMetadata(job.Metadata)); err != nil {
				return fmt.Errorf("carry retry metadata: %w", err)
			}
			_ = logcache.Delete(jobID)

			fmt.Printf("Restarted queued job %s (fresh retry attempt)\n", ids.FormatJobID(jobID))
			fmt.Printf("  Retry budget reset\n")
			fmt.Printf("  Source metadata refreshed; changed sources will be re-synced on dispatch\n")
			printRestartModeLine()
			for _, update := range updates {
				fmt.Printf("  %s\n", update)
			}
			tryResumeRunawayBreaker(database, job)
			return nil
		}

		fmt.Printf("Updated queued job %s\n", ids.FormatJobID(jobID))
		for _, update := range updates {
			fmt.Printf("  %s\n", update)
		}
		tryResumeRunawayBreaker(database, job)
		return nil
	}
	if effectiveStatus == db.StatusRunning || effectiveStatus == db.StatusStarting {
		return fmt.Errorf("job is currently %s; kill it first if you want to retry", effectiveStatus)
	}

	if job.Command == "" {
		return fmt.Errorf("job missing command")
	}

	updates, err := applyRestartOverrides(database, job, overrides)
	if err != nil {
		return err
	}
	if err := validatePinnedHostQueueGate(job.Host, job.GPUClass, job.GPUMemGB); err != nil {
		return err
	}

	// Remove processed tag so the retried job appears in unprocessed listings
	if job.HasTag(db.ProcessedTag) {
		if err := db.RemoveJobTag(database, jobID, db.ProcessedTag); err != nil {
			return fmt.Errorf("remove processed tag: %w", err)
		}
	}

	// Cloud jobs: reset to unplaced (the original instance is gone)
	if job.IsLaunchJob() {
		if err := ops.RefreshProjectDerivedMetadata(database, job.ID, job.WorkingDir, job.Command, job.Inputs); err != nil {
			return err
		}
		if err := db.ResetJobToUnplaced(database, jobID); err != nil {
			return err
		}
		if err := db.SetJobMetadata(database, jobID, persistentAttemptMetadata(job.Metadata)); err != nil {
			return fmt.Errorf("carry retry metadata: %w", err)
		}
		_ = logcache.Delete(jobID)
		fmt.Printf("Reset job %s to queued\n", ids.FormatJobID(jobID))
		printRestartModeLine()
		for _, update := range updates {
			fmt.Printf("  %s\n", update)
		}
		tryResumeRunawayBreaker(database, job)
		return nil
	}

	if !job.HasInventoryHost() {
		// Unplaced terminal jobs can always be retried. Missing host only
		// disqualifies inventory-pinned retries (handled by HasInventoryHost).
		if err := ops.RefreshProjectDerivedMetadata(database, job.ID, job.WorkingDir, job.Command, job.Inputs); err != nil {
			return err
		}
		if err := db.ResetJobToUnplaced(database, jobID); err != nil {
			return err
		}
		if err := db.SetJobMetadata(database, jobID, persistentAttemptMetadata(job.Metadata)); err != nil {
			return fmt.Errorf("carry retry metadata: %w", err)
		}
		_ = logcache.Delete(jobID)
		fmt.Printf("Reset job %s to queued (unplaced job)\n", ids.FormatJobID(jobID))
		printRestartModeLine()
		tryResumeRunawayBreaker(database, job)
		return nil
	}

	// Requeue with same ID (archives the previous run)
	if !requeueableStatuses[effectiveStatus] {
		return fmt.Errorf("cannot retry job with status '%s'; only killed/dead/failed/canceled/completed jobs can be retried", effectiveStatus)
	}

	oldStatus := job.Status

	cfg, relayClient, err := loadCoordinatorRelay()
	if err != nil {
		return err
	}
	if relayEnabled(cfg, relayClient) {
		// Refresh here since the relay path bypasses ops.RequeueJob (which does its own refresh).
		if err := ops.RefreshProjectDerivedMetadata(database, job.ID, job.WorkingDir, job.Command, job.Inputs); err != nil {
			slog.Warn("failed to refresh metadata", "error", err)
		}
		if err := db.RequeueByID(database, jobID); err != nil {
			return fmt.Errorf("update status to queued: %w", err)
		}
		_ = logcache.Delete(jobID)
		ack, err := relayRequeueJob(cfg, relayClient, job)
		if err != nil {
			return err
		}
		fmt.Printf("Restarted job %s via coordinator relay\n", ids.FormatJobID(jobID))
		fmt.Printf("  Status: %s → queued\n", oldStatus)
		printRestartModeLine()
		if ack != nil && ack.Message != "" {
			fmt.Printf("  relay: %s\n", ack.Message)
		}
		return nil
	}

	result, err := ops.RequeueJob(database, job, ops.DefaultOptions())
	if err != nil {
		return err
	}
	if result.Deferred {
		if result.Message != "" {
			fmt.Println(result.Message)
		}
	}

	fmt.Printf("Restarted job %s on %s\n", ids.FormatJobID(jobID), job.Host)
	fmt.Printf("  Status: %s → queued\n", oldStatus)
	printRestartModeLine()
	for _, update := range updates {
		fmt.Printf("  %s\n", update)
	}
	if job.Description != "" {
		fmt.Printf("  Description: %s\n", job.Description)
	}
	if len(job.EnvVars) > 0 {
		fmt.Printf("  Env vars: %s\n", formatEnvVarsForDisplay(job.EnvVars))
	}
	return nil
}

func printRestartModeLine() {
	switch {
	case restartFromScratch:
		fmt.Printf("  Mode: from scratch\n")
	case restartCheckpointed:
		fmt.Printf("  Mode: checkpointed\n")
	}
}

func tryResumeRunawayBreaker(database *sql.DB, job *db.Job) {
	if resumed, err := campaign.ResumeRunawayBreakerForJob(database, job); err != nil {
		slog.Warn("failed to resume runaway breaker", "error", err)
	} else if resumed {
		fmt.Printf("  Auto-launch resumed (was paused due to repeated failures)\n")
	}
}
