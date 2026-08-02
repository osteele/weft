package campaign

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/runner"
	weftsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/vastai"
	"github.com/osteele/weft/internal/workdir"
)

var (
	sendGraceJobPayload      = controlplane.SendGraceJobPayload
	sendGraceJobPayloadNoAck = controlplane.SendGraceJobPayloadNoAck
	sendGraceCancelAttempts  = controlplane.SendGraceCancelAttempts
	uploadSourceToR2         = weftsync.UploadSourceToR2ForInputs
)

// MinGraceRemaining is the minimum grace period remaining to consider an
// instance for reuse. Instances with less time are auto-extended.
const MinGraceRemaining = 5 * time.Minute

// InstanceCapacity describes a reusable cloud instance and its available resources.
type InstanceCapacity struct {
	Instance          *db.Launch
	DiskFreeGB        int
	ProvisionedInputs []string
	RunningJobCount   int
	GraceRemaining    time.Duration // 0 if status=running
}

// ReuseAssignment pairs a job with the instance it should be submitted to.
type ReuseAssignment struct {
	Job      *db.Job
	Instance InstanceCapacity
}

func instanceAcceptsReuse(inst *db.Launch) (bool, string) {
	if inst == nil {
		return false, "instance not found"
	}
	if inst.Status != db.LaunchStatusGrace && inst.Status != db.LaunchStatusRunning {
		return false, fmt.Sprintf("instance %s is not reusable (status=%s)", ids.FormatInstanceID(inst.ID), inst.Status)
	}
	if inst.HasActiveTerminationIntent() {
		return false, fmt.Sprintf("instance %s is self-destructing", ids.FormatInstanceID(inst.ID))
	}
	if inst.Cordoned {
		msg := fmt.Sprintf("instance %s is cordoned", ids.FormatInstanceID(inst.ID))
		if inst.CordonReason != "" {
			msg += " (" + inst.CordonReason + ")"
		}
		return false, msg
	}
	return true, ""
}

func instanceAcceptsReuseWithLiveState(inst *db.Launch, live *db.LaunchLiveState, now time.Time) (bool, string) {
	if ok, reason := instanceAcceptsReuse(inst); !ok {
		return false, reason
	}
	if inst.Status != db.LaunchStatusRunning || inst.AgentReadyAtUnix == nil {
		return true, ""
	}
	if live == nil || live.HeartbeatTS <= 0 {
		return false, fmt.Sprintf("instance %s has no cached heartbeat after agent ready", ids.FormatInstanceID(inst.ID))
	}
	age := now.Sub(time.Unix(live.HeartbeatTS, 0))
	threshold := effectiveHeartbeatStaleThreshold(inst.AgentReadyAtUnix, now)
	if age > threshold {
		return false, fmt.Sprintf("instance %s heartbeat is stale (%s old)", ids.FormatInstanceID(inst.ID), age.Truncate(time.Second))
	}
	return true, ""
}

// NewInstanceCapacity builds an InstanceCapacity for a single launch.
// Returns (cap, true) if the instance is reusable, or (zero, false) if it
// should be skipped (wrong status, self-destructing, expired grace).
func NewInstanceCapacity(inst *db.Launch, runningJobCount int) (InstanceCapacity, bool) {
	if ok, _ := instanceAcceptsReuse(inst); !ok {
		return InstanceCapacity{}, false
	}

	cap := InstanceCapacity{
		Instance:          inst,
		ProvisionedInputs: inst.ProvisionedInputs,
		RunningJobCount:   runningJobCount,
		DiskFreeGB:        estimateDiskFree(inst),
	}

	if inst.Status == db.LaunchStatusGrace && inst.GraceDeadline != nil {
		cap.GraceRemaining = time.Until(time.Unix(*inst.GraceDeadline, 0))
		if cap.GraceRemaining <= 0 {
			return InstanceCapacity{}, false
		}
	}

	return cap, true
}

func newInstanceCapacityFromDB(database *sql.DB, inst *db.Launch, runningJobCount int) (InstanceCapacity, bool, error) {
	cap, ok := NewInstanceCapacity(inst, runningJobCount)
	if !ok {
		return InstanceCapacity{}, false, nil
	}
	free, err := estimateReusableDiskFree(database, inst)
	if err != nil {
		return InstanceCapacity{}, false, err
	}
	cap.DiskFreeGB = free
	return cap, true, nil
}

// FindReusableInstances returns non-terminal cloud instances that could accept new jobs.
// Returns instances with status "grace" or "running".
func FindReusableInstances(database *sql.DB) ([]InstanceCapacity, error) {
	instances, err := db.ListRunningLaunches(database)
	if err != nil {
		return nil, err
	}

	jobCounts, _ := db.GetActiveLaunchJobCounts(database)
	launchIDs := make([]int64, 0, len(instances))
	for _, inst := range instances {
		if inst != nil && inst.ID > 0 {
			launchIDs = append(launchIDs, inst.ID)
		}
	}
	liveStates, err := db.GetLaunchLiveStates(database, launchIDs)
	if err != nil {
		return nil, err
	}
	diskFailedLaunches, err := launchDiskFailureSet(database, launchIDs)
	if err != nil {
		return nil, err
	}
	now := time.Now()

	var result []InstanceCapacity
	for _, inst := range instances {
		if diskFailedLaunches[inst.ID] {
			continue
		}
		if ok, _ := instanceAcceptsReuseWithLiveState(inst, liveStates[inst.ID], now); !ok {
			continue
		}
		cap, ok, err := newInstanceCapacityFromDB(database, inst, jobCounts[inst.ID])
		if err != nil {
			return nil, err
		}
		if ok {
			result = append(result, cap)
		}
	}

	return result, nil
}

func launchDiskFailureSet(database *sql.DB, launchIDs []int64) (map[int64]bool, error) {
	result := make(map[int64]bool)
	for _, launchID := range launchIDs {
		var count int
		err := database.QueryRow(
			`SELECT COUNT(*)
			   FROM job_attempts
			  WHERE launch_id = ?
			    AND (
			      failure_reason = 'disk_full'
			      OR lower(COALESCE(failure_reason, '')) LIKE '%edquot%'
			      OR lower(COALESCE(error_message, '')) LIKE '%edquot%'
			      OR lower(COALESCE(error_message, '')) LIKE '%os error 122%'
			      OR lower(COALESCE(error_message, '')) LIKE '%filesystem quota%'
			      OR lower(COALESCE(error_message, '')) LIKE '%quota exceeded%'
			    )`,
			launchID,
		).Scan(&count)
		if err != nil {
			return nil, err
		}
		result[launchID] = count > 0
	}
	return result, nil
}

// estimateDiskFree estimates the free disk space on an instance.
func estimateDiskFree(inst *db.Launch) int {
	if inst.DiskGB == 0 {
		return 0 // unknown disk size, can't estimate
	}

	overhead := BaseOverheadGB + CUDAOverheadGB // conservative: assume CUDA
	usedByInputs := estimateInputsDisk(inst.ProvisionedInputs)

	free := inst.DiskGB - overhead - usedByInputs
	if free < 0 {
		return 0
	}
	return free
}

func estimateReusableDiskFree(database *sql.DB, inst *db.Launch) (int, error) {
	free := estimateDiskFree(inst)
	if database == nil || inst == nil || inst.ID == 0 || inst.DiskGB == 0 {
		return free, nil
	}
	jobs, err := db.GetLaunchJobsIncludingAttempts(database, inst.ID)
	if err != nil {
		return 0, fmt.Errorf("list jobs for %s disk accounting: %w", ids.FormatInstanceID(inst.ID), err)
	}
	if len(jobs) == 0 {
		return free, nil
	}

	provisionedInputs := append([]string(nil), inst.ProvisionedInputs...)
	setupByProject := map[string]int{}
	cap := InstanceCapacity{
		Instance:          inst,
		DiskFreeGB:        free,
		ProvisionedInputs: provisionedInputs,
	}
	extraInputsGB := 0
	runtimeGB := 0
	for _, job := range jobs {
		if !launchJobConsumesDisk(job) {
			continue
		}
		incremental := subtractInputs(job.Inputs, provisionedInputs)
		extraInputsGB += estimateInputsDisk(incremental)
		for _, input := range incremental {
			if !slices.Contains(provisionedInputs, input) {
				provisionedInputs = append(provisionedInputs, input)
			}
		}
		cap.ProvisionedInputs = provisionedInputs

		setupGB, _ := estimateReuseSetupDiskGB(job, cap, nil)
		key := reuseSetupDiskKey(job)
		if key == "" {
			runtimeGB += setupGB
		} else if setupGB > setupByProject[key] {
			setupByProject[key] = setupGB
		}
		runtimeGB += EstimateRuntimeDiskGB(job)
	}

	setupGB := 0
	for _, gb := range setupByProject {
		setupGB += gb
	}
	// estimateDiskFree already reserves one CUDA-sized setup/cache budget.
	// Reused instances need additional budgets for other projects that have
	// already left virtualenvs and package caches behind.
	setupGB = max(0, setupGB-CUDAOverheadGB)

	free -= extraInputsGB + setupGB + runtimeGB
	if free < 0 {
		return 0, nil
	}
	return free, nil
}

func launchJobConsumesDisk(job *db.Job) bool {
	if job == nil {
		return false
	}
	switch job.Status {
	case db.StatusCanceled, db.StatusKilled, db.StatusDraft:
		return false
	default:
		return true
	}
}

func reuseSetupDiskKey(job *db.Job) string {
	if job == nil {
		return ""
	}
	dir := workdir.ResolveLocal(job.EffectiveWorkingDir())
	if dir == "" {
		return ""
	}
	return dir
}

// estimateInputsDisk estimates disk usage for a set of input refs in GB.
func estimateInputsDisk(inputs []string) int {
	if len(inputs) == 0 {
		return 0
	}
	totalBytes, _, _ := dataloc.ResolveInputSizes(inputs, nil)
	if totalBytes == 0 {
		return 0
	}
	return int(math.Ceil(float64(totalBytes) / 1e9 * HFCacheMultiplier))
}

// MatchJobToInstance checks whether a job is compatible with an instance.
// Returns (compatible, reason) where reason explains why it's not compatible.
func MatchJobToInstance(job *db.Job, cap InstanceCapacity) (bool, string) {
	return matchJobToInstance(job, cap, nil)
}

// MatchJobToInstanceWithUV checks compatibility including UV sync disk needs.
// When r2Client is non-nil, estimates the incremental UV sync download size
// and includes it in the disk check.
func MatchJobToInstanceWithUV(job *db.Job, cap InstanceCapacity, r2Client *r2.Client) (bool, string) {
	return matchJobToInstance(job, cap, r2Client)
}

func matchJobToInstance(job *db.Job, cap InstanceCapacity, r2Client *r2.Client) (bool, string) {
	inst := cap.Instance

	// Ahead of ConstraintsFromJob: a pin rejects on a map lookup, while
	// resolving constraints scans uv.lock and pyproject.toml uncached, and this
	// runs per job×instance on every autopilot tick.
	if ok, reason := matchMachineAffinityIntent(job, inst); !ok {
		return false, reason
	}
	if ok, reason := matchProviderIntent(job, inst); !ok {
		return false, reason
	}

	constraints := placement.ConstraintsFromJob(job)
	if ok, reason := matchInstanceTypeIntent(job, inst); !ok {
		return false, reason
	}
	if ok, reason := matchRunpodCloudTypeIntent(job, inst); !ok {
		return false, reason
	}
	if ok, reason := matchJobRequiredImage(job, inst); !ok {
		return false, reason
	}
	if job.HasTag(db.TagCPUIntensive) {
		floor := computeCPUCoresFloor()
		if inst.CPUCores <= 0 {
			return false, "CPU cores unknown for cpu-intensive"
		}
		if inst.CPUCores < floor {
			return false, fmt.Sprintf("CPU cores insufficient for cpu-intensive: need=%d instance=%d", floor, inst.CPUCores)
		}
	}

	if ok, reason := matchInstanceTargetEligibility(placement.Constraints{GPUClass: constraints.GPUClass}, inst, 0); !ok {
		return false, reason
	}
	if broadNVIDIAConstraint(constraints.GPUClass) && premiumAcceleratorClass(inst.GPUClass, inst.ResolvedGPUName) {
		return false, fmt.Sprintf("broad NVIDIA job should not reuse premium accelerator: job=%s instance=%s", constraints.GPUClass, inst.DisplayGPUBrief())
	}

	// GPU memory check. IntendedMemGB resolves the persisted gpu_mem_gb
	// to the user's original intent — rolling back the +2GB submit-time
	// headroom only when the stored value is recognizably (ceiling +
	// defaultHeadroom) for a known model. A job stored as "82" for "A100
	// 80GB" therefore matches an instance reporting 80GB instead of
	// failing with "insufficient", but a stored value of "50" stays "50"
	// (no spurious +2GB cushion at the capacity comparison boundary).
	jobMemGB := 0
	if constraints.GPUMemGB > 0 {
		jobMemGB = vastai.IntendedMemGB(constraints.GPUClass, constraints.GPUMemGB)
	}
	reuseConstraints := constraints
	if jobMemGB > 0 {
		reuseConstraints.GPUMemGB = jobMemGB
	}
	if ok, reason := matchInstanceTargetEligibility(reuseConstraints, inst, placementIntentForJob(job).NumGPUs); !ok {
		return false, reason
	}

	// Disk check: compute incremental inputs (HF models) + setup scratch.
	if cap.DiskFreeGB > 0 || inst.DiskGB > 0 {
		need := estimateReuseJobDiskNeedGB(job, cap, r2Client)
		if need.diskFloorGB > 0 && inst.DiskGB > 0 && need.diskFloorGB > inst.DiskGB {
			return false, fmt.Sprintf("disk insufficient: disk floor=%dGB instance=%dGB", need.diskFloorGB, inst.DiskGB)
		}
		if need.totalGB > 0 && need.totalGB > cap.DiskFreeGB {
			detail := ""
			if need.reason != "" {
				detail = " (" + need.reason + ")"
			}
			return false, fmt.Sprintf("disk insufficient: need=%dGB free=%dGB%s", need.totalGB, cap.DiskFreeGB, detail)
		}
	}

	// Grace deadline check
	if cap.GraceRemaining > 0 && cap.GraceRemaining < MinGraceRemaining {
		return false, fmt.Sprintf("grace period too short: %s remaining", cap.GraceRemaining.Truncate(time.Second))
	}

	return true, ""
}

// matchMachineAffinityIntent rejects a reusable instance that is not on a
// machine the job is pinned to. A pin is a property of the job, so it is checked
// here with the job's other constraints rather than where instances happen to be
// enumerated — the launch-level pin is filtered separately, in
// filterReusableByMachineAffinity, and the two compose to an intersection.
//
// Unknown fails closed, as at the claim boundary; see
// db.assertMachineAffinitySatisfied for why.
func matchMachineAffinityIntent(job *db.Job, inst *db.Launch) (bool, string) {
	if job == nil || inst == nil || job.CLIResourceOverrides == nil ||
		len(job.CLIResourceOverrides.MachineAffinity) == 0 {
		return true, ""
	}
	key := db.ProviderMachineKey(string(inst.Provider), inst.MachineID)
	if key == "" {
		return false, "job is pinned to a machine but the instance reports none"
	}
	if !db.MachineRefsMatch(job.CLIResourceOverrides.MachineAffinity, key) {
		return false, fmt.Sprintf("job is pinned to another machine: instance=%s", key)
	}
	return true, ""
}

func matchProviderIntent(job *db.Job, inst *db.Launch) (bool, string) {
	if job == nil || inst == nil {
		return true, ""
	}
	provider, ok := db.RequestedProvider(job.Tags)
	if !ok || provider == "" {
		return true, ""
	}
	if strings.EqualFold(strings.TrimSpace(inst.Provider), provider) {
		return true, ""
	}
	return false, fmt.Sprintf("provider mismatch: job=%s instance=%s", provider, strings.TrimSpace(inst.Provider))
}

func matchInstanceTypeIntent(job *db.Job, inst *db.Launch) (bool, string) {
	if job == nil || inst == nil {
		return true, ""
	}
	if inst.InstanceType == cloud.InstanceTypeInterruptible && !job.UsesPreemptiblePlacement() {
		return false, "instance is interruptible but job is not tagged interruptible"
	}
	return true, ""
}

func matchRunpodCloudTypeIntent(job *db.Job, inst *db.Launch) (bool, string) {
	if job == nil || inst == nil {
		return true, ""
	}
	requested := strings.TrimSpace(job.RequestedRunpodCloudType())
	if requested == "" {
		return true, ""
	}
	if !strings.EqualFold(strings.TrimSpace(inst.Provider), string(cloud.ProviderRunpod)) {
		return false, fmt.Sprintf("RunPod cloud type %s requires a RunPod instance", requested)
	}
	actual := strings.TrimSpace(inst.RunpodCloudType)
	if actual == "" {
		return false, fmt.Sprintf("RunPod cloud type unknown: job=%s", requested)
	}
	if strings.EqualFold(actual, requested) {
		return true, ""
	}
	return false, fmt.Sprintf("RunPod cloud type mismatch: job=%s instance=%s", requested, actual)
}

func matchJobRequiredImage(job *db.Job, inst *db.Launch) (bool, string) {
	if job == nil || inst == nil {
		return true, ""
	}
	required := jobRequiredReuseImage(job, inst.Provider)
	if strings.TrimSpace(required) == "" {
		return true, ""
	}
	actual := strings.TrimSpace(inst.DockerImage)
	if ImagesCompatible(required, actual) {
		return true, ""
	}
	return false, fmt.Sprintf("image incompatible: job requires %s instance has %s",
		formatResolvedImageForReuse(required, inst.Provider),
		formatResolvedImageForReuse(actual, inst.Provider))
}

func jobRequiredReuseImage(job *db.Job, provider string) string {
	if job == nil {
		return ""
	}
	localDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
	image, _, _ := ResolveJobImageSettings(localDir, job.Command)
	image = strings.TrimSpace(image)
	if image == "" {
		return ""
	}
	if strings.EqualFold(provider, string(cloud.ProviderRunpod)) {
		return normalizeRunpodGroupImage(image)
	}
	return image
}

func formatResolvedImageForReuse(image, provider string) string {
	image = strings.TrimSpace(image)
	if image != "" {
		return image
	}
	if strings.EqualFold(provider, string(cloud.ProviderRunpod)) {
		return cloud.DefaultRunpodImage
	}
	return cloud.DefaultImage
}

func matchPlacementCompatibility(constraints placement.Constraints, inst *db.Launch) (bool, string) {
	if inst == nil {
		return true, ""
	}
	if !constraints.HasGPURuntimeBounds() {
		return true, ""
	}
	return matchInstanceTargetEligibility(constraints, inst, constraints.NumGPUs)
}

func matchInstanceTargetEligibility(constraints placement.Constraints, inst *db.Launch, requestedGPUs int) (bool, string) {
	if inst == nil {
		return true, ""
	}
	evalConstraints := constraints
	// Reuse records do not currently persist NVIDIA driver major. Provider and
	// image-specific driver checks stay in the cloud-offer/provider filters.
	evalConstraints.MinDriverVersion = 0
	evalConstraints.VersionRequirements = nil
	if evalConstraints.NumGPUs > 1 && inst.NumGPUs <= 0 {
		evalConstraints.NumGPUs = 1
	}
	verdict := placement.EvaluateEligibility(evalConstraints, TargetSpecFromCloudInstance(*inst))
	if verdict.Eligible {
		return true, ""
	}
	return false, instanceEligibilityReason(verdict, constraints, inst, requestedGPUs)
}

func instanceEligibilityReason(verdict placement.Verdict, constraints placement.Constraints, inst *db.Launch, requestedGPUs int) string {
	if len(verdict.Reasons) == 0 {
		return "placement compatibility mismatch"
	}
	reason := verdict.Reasons[0]
	switch reason.Kind {
	case placement.ReasonGPUClass:
		return fmt.Sprintf("GPU class mismatch: job=%s instance=%s", constraints.GPUClass, inst.GPUClass)
	case placement.ReasonGPUCount:
		if requestedGPUs <= 0 {
			requestedGPUs = constraints.NumGPUs
		}
		return fmt.Sprintf("GPU count insufficient: job=%d instance=%d", requestedGPUs, inst.NumGPUs)
	case placement.ReasonGPUMemory:
		return fmt.Sprintf("GPU memory insufficient: job=%dGB instance=%dGB", constraints.GPUMemGB, inst.GPUMemGB)
	case placement.ReasonComputeCapMax:
		gpuCap := instanceComputeCap(inst)
		if gpuCap == "" {
			return fmt.Sprintf("compute capability unknown; excluded under max cap %s", constraints.MaxComputeCap)
		}
		return fmt.Sprintf("compute capability too new: job<=%s instance=%s", constraints.MaxComputeCap, gpuCap)
	case placement.ReasonComputeCapMin:
		gpuCap := instanceComputeCap(inst)
		if gpuCap == "" {
			return fmt.Sprintf("compute capability unknown: need>=%s", constraints.MinComputeCap)
		}
		return fmt.Sprintf("compute capability too old: job>=%s instance=%s", constraints.MinComputeCap, gpuCap)
	case placement.ReasonCompatibility, placement.ReasonCUDAChain:
		if constraints.MinCUDAVersion != "" {
			if inst.CUDAVersion <= 0 {
				return fmt.Sprintf("CUDA compatibility unknown: need>=%s", constraints.MinCUDAVersion)
			}
			return fmt.Sprintf("CUDA compatibility insufficient: need>=%s instance=%.1f", constraints.MinCUDAVersion, inst.CUDAVersion)
		}
	}
	return reason.Message
}

func instanceComputeCap(inst *db.Launch) string {
	if inst == nil {
		return ""
	}
	if cap := placement.ComputeCapForGPU(inst.ResolvedGPUName); cap != "" {
		return cap
	}
	return placement.ComputeCapForGPU(inst.GPUClass)
}

type reuseDiskNeed struct {
	totalGB     int
	diskFloorGB int
	reason      string
}

func estimateReuseJobDiskNeedGB(job *db.Job, cap InstanceCapacity, r2Client *r2.Client) reuseDiskNeed {
	if job == nil {
		return reuseDiskNeed{}
	}

	inputGB := estimateInputsDisk(subtractInputs(job.Inputs, cap.ProvisionedInputs))
	setupGB, setupReason := estimateReuseSetupDiskGB(job, cap, r2Client)
	runtimeGB := EstimateRuntimeDiskGB(job)
	scriptEnvGB := commandDepIncrementalEnvGB(job)
	floorGB := jobDiskFloorGB(job)

	reason := setupReason
	if scriptEnvGB > 0 {
		envReason := fmt.Sprintf("isolated script env estimate=%dGB", scriptEnvGB)
		if reason == "" {
			reason = envReason
		} else {
			reason += "; " + envReason
		}
	}

	return reuseDiskNeed{
		totalGB:     inputGB + setupGB + runtimeGB + scriptEnvGB,
		diskFloorGB: floorGB,
		reason:      reason,
	}
}

func estimateReuseSetupDiskGB(job *db.Job, cap InstanceCapacity, r2Client *r2.Client) (int, string) {
	localDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
	if localDir == "" {
		return 0, ""
	}
	sourceDirs := []string{localDir}
	if bytes, ok := estimateGroupUVBytesFromManifest(sourceDirs, r2Client); ok {
		return int(math.Ceil(float64(bytes) / 1e9)), ""
	}
	if !hasCUDAPackages(sourceDirs) {
		return NonCUDAOverheadGB, "setup estimate from non-CUDA dependency fallback"
	}

	image := ""
	if cap.Instance != nil {
		image = cap.Instance.DockerImage
	}
	if isPyTorchImage(image) {
		if pyTorchImageShortcutLikely(localDir) {
			return CUDAOverheadWithPyTorchImageGB, "setup estimate assumes PyTorch image provides torch"
		}
		// The image supplies CUDA and torch, but its torch is built for the
		// image's Python (3.11). A job on a different Python cannot reuse that
		// torch and must reinstall it (with its bundled CUDA), so the full
		// overhead applies — this is a Python-version mismatch, not a missing
		// CUDA runtime.
		if pyReq, ok := pythonVersionRequestForReuse(localDir); ok && pyReq != "" {
			return CUDAOverheadGB, fmt.Sprintf("image %s ships Python 3.11 but this job requires Python %s; its prebuilt torch cannot be reused and must be reinstalled", image, pyReq)
		}
		return CUDAOverheadGB, fmt.Sprintf("image %s prebuilt torch does not match this job's Python; torch must be reinstalled", image)
	}
	if image == "" {
		image = cloud.DefaultImage
	}
	return CUDAOverheadGB, fmt.Sprintf("CUDA packages not provided by image %s", image)
}

func estimateGroupUVBytesFromManifest(sourceDirs []string, r2Client *r2.Client) (int64, bool) {
	lockfileHashes := estimate.LockfileHash(sourceDirs)
	if len(lockfileHashes) == 0 {
		return 0, false
	}
	manifests := estimate.FetchUVManifests(r2Client, lockfileHashes, "linux-amd64")
	if len(manifests) == 0 {
		return 0, false
	}
	return estimate.EstimateUVSyncBytes(manifests), true
}

func pyTorchImageShortcutLikely(localDir string) bool {
	req, ok := pythonVersionRequestForReuse(localDir)
	if !ok {
		return true
	}
	return req == "3.11"
}

func pythonVersionRequestForReuse(localDir string) (string, bool) {
	data, err := os.ReadFile(path.Join(localDir, ".python-version"))
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	if line == "" {
		return "", false
	}
	if idx := strings.Index(line, ":"); idx >= 0 {
		return "", true
	}
	parts := strings.Split(line, ".")
	if len(parts) < 2 {
		return "", true
	}
	return parts[0] + "." + parts[1], true
}

func jobDiskFloorGB(job *db.Job) int {
	if job == nil {
		return 0
	}
	switch {
	case job.CLIResourceOverrides != nil && job.CLIResourceOverrides.DiskGB != nil:
		return max(0, *job.CLIResourceOverrides.DiskGB)
	case job.Metadata != nil && job.Metadata.Disk != nil && job.Metadata.Disk.DiskGB > 0:
		return job.Metadata.Disk.DiskGB
	default:
		if meta := scanJobScriptMeta(job); meta != nil && meta.DiskGB > 0 {
			return meta.DiskGB
		}
	}
	return 0
}

// subtractInputs returns inputs in job that are not in provisioned (set difference).
func subtractInputs(jobInputs, provisioned []string) []string {
	if len(provisioned) == 0 {
		return jobInputs
	}
	have := make(map[string]struct{}, len(provisioned))
	for _, ref := range provisioned {
		have[ref] = struct{}{}
	}
	var result []string
	for _, ref := range jobInputs {
		if _, ok := have[ref]; !ok {
			result = append(result, ref)
		}
	}
	return result
}

// RankForJob sorts compatible instances: grace first (free), then by data locality
// (more provisioned inputs = better). Returns only compatible instances.
func RankForJob(job *db.Job, instances []InstanceCapacity) []InstanceCapacity {
	return RankForJobWithPreferredIDs(job, instances, nil)
}

// RankForJobWithPreferredIDs sorts compatible instances while allowing
// preferred instances to exceed the generic reuse queue cap. This is used for
// --needs producer co-location: the consumer must sit behind the producer on
// the same rental queue so it can start automatically when the artifact exists.
func RankForJobWithPreferredIDs(job *db.Job, instances []InstanceCapacity, preferredIDs map[int64]bool) []InstanceCapacity {
	var compatible []InstanceCapacity
	for _, cap := range instances {
		if ok, _ := MatchJobToInstance(job, cap); ok {
			compatible = append(compatible, cap)
		}
	}

	// Sort: grace before running, then by overlap count (descending)
	sort.Slice(compatible, func(i, j int) bool {
		return rankBetter(compatible[i], compatible[j], job)
	})

	return compatible
}

// rankBetter returns true if a is a better choice than b for the given job.
func rankBetter(a, b InstanceCapacity, job *db.Job) bool {
	aGrace := a.Instance.Status == db.LaunchStatusGrace
	bGrace := b.Instance.Status == db.LaunchStatusGrace

	// Grace instances first (free)
	if aGrace && !bGrace {
		return true
	}
	if !aGrace && bGrace {
		return false
	}

	// More overlapping inputs = better data locality
	aOverlap := countOverlap(job.Inputs, a.ProvisionedInputs)
	bOverlap := countOverlap(job.Inputs, b.ProvisionedInputs)
	if aOverlap != bOverlap {
		return aOverlap > bOverlap
	}

	// Fewer running jobs = less waiting
	return a.RunningJobCount < b.RunningJobCount
}

// countOverlap counts how many items in a are also in b.
func countOverlap(a, b []string) int {
	if len(b) == 0 {
		return 0
	}
	set := make(map[string]struct{}, len(b))
	for _, s := range b {
		set[s] = struct{}{}
	}
	count := 0
	for _, s := range a {
		if _, ok := set[s]; ok {
			count++
		}
	}
	return count
}

// PlanReuse assigns jobs to reusable instances, returning assignments and remaining jobs.
// Tries grace instances first (free), then running instances (shared cost).
// Mutates the instances slice to track queue depth, data locality, and disk
// budget after each planned assignment.
func PlanReuse(jobs []*db.Job, instances []InstanceCapacity) ([]ReuseAssignment, []*db.Job) {
	var assignments []ReuseAssignment
	var remaining []*db.Job

	for _, job := range jobs {
		ranked := RankForJob(job, instances)
		if len(ranked) == 0 {
			remaining = append(remaining, job)
			continue
		}

		best := ranked[0]
		assignments = append(assignments, ReuseAssignment{Job: job, Instance: best})

		for i := range instances {
			if instances[i].Instance.ID == best.Instance.ID {
				consumeReuseJob(&instances[i], job)
				break
			}
		}
	}

	return assignments, remaining
}

func consumeReuseJob(cap *InstanceCapacity, job *db.Job) {
	if cap == nil || job == nil {
		return
	}

	incremental := subtractInputs(job.Inputs, cap.ProvisionedInputs)
	for _, input := range incremental {
		if !slices.Contains(cap.ProvisionedInputs, input) {
			cap.ProvisionedInputs = append(cap.ProvisionedInputs, input)
		}
	}

	need := estimateReuseJobDiskNeedGB(job, *cap, nil)
	cap.DiskFreeGB -= need.totalGB
	if cap.DiskFreeGB < 0 {
		cap.DiskFreeGB = 0
	}
	cap.RunningJobCount++
}

// FormatReuseAssignments returns a human-readable summary of reuse assignments.
func FormatReuseAssignments(assignments []ReuseAssignment) string {
	if len(assignments) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("=== Reuse existing instances ===\n")
	for _, a := range assignments {
		inst := a.Instance.Instance
		detail := ""
		if inst.Status == db.LaunchStatusGrace {
			remaining := a.Instance.GraceRemaining.Truncate(time.Second)
			detail = fmt.Sprintf("grace — %s remaining", remaining)
		} else {
			jobsAhead := max(0, a.Instance.RunningJobCount-1)
			if jobsAhead > 0 {
				detail = fmt.Sprintf("running — %d job(s) ahead, ~%s wait", jobsAhead, formatReuseWaitEstimate(jobsAhead))
			} else {
				detail = "running — no queued jobs ahead"
			}
		}
		cost := "$0.00"
		if inst.Status == db.LaunchStatusRunning {
			cost = "$0.00 (shared)"
		}
		b.WriteString(fmt.Sprintf("  Job #%d  →  Instance %s (%s, %s)  %s\n",
			a.Job.ID, ids.FormatInstanceID(inst.ID), inst.DisplayGPUSpec(), detail, cost))
	}
	return b.String()
}

func formatReuseWaitEstimate(jobsAhead int) string {
	if jobsAhead <= 0 {
		return "0m"
	}
	d := time.Duration(jobsAhead) * 30 * time.Minute
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%02dm", h, m)
}

// GracePayload is the JSON structure written to R2 for job submission.
type GracePayload struct {
	Jobs    []cloud.AgentJob            `json:"jobs"`
	Sources []controlplane.SourceUpdate `json:"sources,omitempty"`
}

// SubmitJobsToInstance submits one or more jobs to an existing cloud instance
// via R2 grace protocol. Works for both grace and running instances. The
// jobs must not be claimed by another active launch — see
// SubmitJobsToInstanceForMove for the move-path variant that opens a hidden
// target attempt before handoff.
func SubmitJobsToInstance(ctx context.Context, database *sql.DB, r2Client *r2.Client, instanceID int64, jobs []*db.Job) error {
	return submitJobsToInstanceImpl(ctx, database, r2Client, instanceID, jobs, false)
}

// SubmitJobsToInstanceForMove is the move-path counterpart of
// SubmitJobsToInstance: it opens hidden target attempts under each job's
// MoveIntent, submits them to the destination, then confirms the handoff.
// The autopilot exclusion invariant relies on the intent being open before
// this is called.
func SubmitJobsToInstanceForMove(ctx context.Context, database *sql.DB, r2Client *r2.Client, instanceID int64, jobs []*db.Job) error {
	return submitJobsToInstanceImpl(ctx, database, r2Client, instanceID, jobs, true)
}

// SendCancelAttempts asks a source launch to drop superseded attempts after a
// move destination has accepted the job.
func SendCancelAttempts(ctx context.Context, r2Client *r2.Client, launchID int64, attemptIDs []int64) error {
	if r2Client == nil || launchID <= 0 || len(attemptIDs) == 0 {
		return nil
	}
	return sendGraceCancelAttempts(ctx, r2Client, launchID, attemptIDs)
}

// ErrMalformedNeedsSpec marks a --needs spec that cannot be parsed. Callers
// that skip jobs on validation failure (the rebalance candidate filter) use
// it to surface permanent user errors louder than routine waits.
var ErrMalformedNeedsSpec = errors.New("malformed --needs spec")

// ValidateJobNeedsReadyForCloud checks that every job-output --needs spec on
// the job could be resolved by the destination submit path. It mirrors
// ClassifyNeedsForLaunch's carve-outs: named assets, on-prem producers
// (host-pinned at submission), and producers co-located on the same live
// target instance (CloudAfter) are exempt. Only cross-instance R2-staged
// needs require a completed producer; the submit path remains the authority
// for R2 artifact existence. targetInstanceID = 0 means "no particular
// destination" and applies the strict cross-instance rule to every producer.
func ValidateJobNeedsReadyForCloud(database *sql.DB, job *db.Job, targetInstanceID int64) error {
	if database == nil || job == nil {
		return nil
	}
	if fresh, err := db.GetJobByID(database, job.ID); err == nil && fresh != nil {
		job.Needs = fresh.Needs
	}
	if len(job.Needs) == 0 {
		return nil
	}
	for _, raw := range job.Needs {
		need, err := runner.ParseNeedsSpec(raw)
		if err != nil {
			return fmt.Errorf("%w: %q for job %s: %v", ErrMalformedNeedsSpec, raw, ids.FormatJobID(job.ID), err)
		}
		if need.IsAsset() {
			continue
		}
		producer, err := db.GetJobByID(database, need.Version)
		if err != nil {
			return fmt.Errorf("check producer %s for job %s --needs %q: %w", ids.FormatJobID(need.Version), ids.FormatJobID(job.ID), raw, err)
		}
		if producer == nil {
			return fmt.Errorf("job %s requires %q that producer %s was not found", ids.FormatJobID(job.ID), raw, ids.FormatJobID(need.Version))
		}
		if producer.HasInventoryHost() {
			continue
		}
		if isSameLiveInstance(database, producer, targetInstanceID) {
			continue
		}
		if producer.EffectiveStatus() != db.StatusCompleted {
			return fmt.Errorf("job %s requires %q that producer %s has not completed (status=%s)",
				ids.FormatJobID(job.ID), raw, ids.FormatJobID(producer.ID), producer.EffectiveStatus())
		}
	}
	return nil
}

func submitJobsToInstanceImpl(ctx context.Context, database *sql.DB, r2Client *r2.Client, instanceID int64, jobs []*db.Job, transfer bool) error {
	inst, err := db.GetLaunch(database, instanceID)
	if err != nil {
		return fmt.Errorf("get instance %s: %w", ids.FormatInstanceID(instanceID), err)
	}
	if ok, reason := instanceAcceptsReuse(inst); !ok {
		return fmt.Errorf("instance %s cannot accept reused jobs: %s", ids.FormatInstanceID(instanceID), reason)
	}
	if ok, reason := instanceAcceptsReuseWithCachedLiveState(database, inst, time.Now()); !ok {
		return fmt.Errorf("instance %s cannot accept reused jobs: %s", ids.FormatInstanceID(instanceID), reason)
	}

	// Validate sources BEFORE claiming anything: a deterministic rejection
	// (no local source, source over the size cap) discovered after the claim
	// costs an attempt row per retry — the wj2812 churn pattern.
	for _, job := range jobs {
		if err := ValidateJobSourceForCloud(job); err != nil {
			return err
		}
		if err := ValidateJobNeedsReadyForCloud(database, job, instanceID); err != nil {
			return err
		}
	}
	if err := validateJobsFitReusableInstance(database, inst, jobs, r2Client); err != nil {
		return err
	}

	payload := GracePayload{
		Sources: []controlplane.SourceUpdate{},
	}

	// Sort jobs by scheduling intent so the agent executes priority jobs first.
	sort.SliceStable(jobs, func(i, j int) bool {
		return db.SchedulingLess(jobs[i], jobs[j])
	})

	claimedJobs := make([]*db.Job, 0, len(jobs))
	claimedJobIDs := make([]int64, 0, len(jobs))
	type moveClaim struct {
		intentID       int64
		jobID          int64
		attemptID      int64
		sourceLaunchID int64
	}
	moveClaims := make([]moveClaim, 0, len(jobs))
	rollbackClaims := func() error {
		if !transfer {
			return resetClaimedJobsToUnplaced(database, claimedJobIDs)
		}
		for _, claim := range moveClaims {
			_ = db.AbandonMoveLoser(database, claim.intentID, claim.attemptID, db.AttemptAbandonedMoveDestinationRejected)
			_ = db.ResolveMoveIntent(database, claim.intentID, db.MoveIntentStateCanceled, "destination did not accept")
		}
		return nil
	}
	// Per-source-launch attempt-ids that should be canceled on the source
	// agent once the transfer succeeds. Populated only when transfer=true.
	cancelByLaunch := map[int64][]int64{}
	for _, job := range jobs {
		var prior PriorAttempt
		var claimedJob *db.Job
		if transfer {
			intent, err := db.GetOpenMoveIntent(database, job.ID)
			if err != nil {
				return fmt.Errorf("get open move intent for job %s: %w", ids.FormatJobID(job.ID), err)
			}
			if intent == nil {
				return fmt.Errorf("job %s has no open move intent", ids.FormatJobID(job.ID))
			}
			prior = priorAttemptFromMoveIntent(intent)
			attemptID, err := db.CreateMoveTargetAttempt(database, intent.ID, job.ID, "", &instanceID, db.StatusQueued)
			if err != nil {
				rollbackErr := rollbackClaims()
				if rollbackErr != nil {
					return fmt.Errorf("create move target attempt for job %s on instance %s: %w (rollback: %v)", ids.FormatJobID(job.ID), ids.FormatInstanceID(instanceID), err, rollbackErr)
				}
				return fmt.Errorf("create move target attempt for job %s on instance %s: %w", ids.FormatJobID(job.ID), ids.FormatInstanceID(instanceID), err)
			}
			moveClaims = append(moveClaims, moveClaim{
				intentID:       intent.ID,
				jobID:          job.ID,
				attemptID:      attemptID,
				sourceLaunchID: prior.LaunchID,
			})
			copyJob := *job
			copyJob.Host = ""
			copyJob.LaunchID = &instanceID
			copyJob.LatestRunID = &attemptID
			copyJob.Status = db.StatusQueued
			claimedJob = &copyJob
		} else {
			var err error
			claimedJob, err = claimJobForLaunchWithOpts(database, job.ID, instanceID, false)
			if err != nil {
				rollbackErr := rollbackClaims()
				if rollbackErr != nil {
					return fmt.Errorf("claim job %s for instance %s: %w (rollback: %v)", ids.FormatJobID(job.ID), ids.FormatInstanceID(instanceID), err, rollbackErr)
				}
				return fmt.Errorf("claim job %s for instance %s: %w", ids.FormatJobID(job.ID), ids.FormatInstanceID(instanceID), err)
			}
		}
		claimedJobs = append(claimedJobs, claimedJob)
		claimedJobIDs = append(claimedJobIDs, claimedJob.ID)
		if prior.LaunchID > 0 && prior.LaunchID != instanceID && prior.AttemptID > 0 {
			cancelByLaunch[prior.LaunchID] = append(cancelByLaunch[prior.LaunchID], prior.AttemptID)
		}
	}
	opCtx, opCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer opCancel()

	// Upload sources and build payload
	for _, job := range claimedJobs {
		sourceDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
		if sourceDir == "" || workdir.IsContainerPath(sourceDir) {
			rollbackErr := rollbackClaims()
			err := fmt.Errorf("job %s has no local source directory (working_dir=%q)", ids.FormatJobID(job.ID), job.EffectiveWorkingDir())
			if rollbackErr != nil {
				return fmt.Errorf("%w (rollback: %v)", err, rollbackErr)
			}
			return err
		}

		// Upload fresh sources (content-addressed, so deduped)
		slog.Debug("source upload: reuse path", "component", "reuse",
			"jobID", job.ID, "sourceDir", sourceDir, "inputCount", len(job.Inputs), "inputs", job.Inputs)
		sourceR2Key, err := uploadSourceToR2(opCtx, r2Client, sourceDir, job.Inputs)
		if err != nil {
			rollbackErr := rollbackClaims()
			if rollbackErr != nil {
				return fmt.Errorf("upload source for job %s: %w (rollback: %v)", ids.FormatJobID(job.ID), err, rollbackErr)
			}
			return fmt.Errorf("upload source for job %s: %w", ids.FormatJobID(job.ID), err)
		}

		// Compute remote working directory under the synced project root.
		remoteDir := path.Join(cloud.ProjectRootDir, path.Base(sourceDir))

		agentJob, err := newCloudAgentJob(job, remoteDir)
		if err != nil {
			rollbackErr := rollbackClaims()
			if rollbackErr != nil {
				return fmt.Errorf("build agent job payload for job %s: %w (rollback: %v)", ids.FormatJobID(job.ID), err, rollbackErr)
			}
			return fmt.Errorf("build agent job payload for job %s: %w", ids.FormatJobID(job.ID), err)
		}
		cloudNeeds, cloudAfter, err := resolveCloudNeedsForJob(opCtx, database, r2Client, job, instanceID)
		if err != nil {
			rollbackErr := rollbackClaims()
			if rollbackErr != nil {
				return fmt.Errorf("%w (rollback: %v)", err, rollbackErr)
			}
			return err
		}
		checkpointNeeds, err := resolveTransportableCheckpointNeeds(opCtx, database, r2Client, job)
		if err != nil {
			rollbackErr := rollbackClaims()
			if rollbackErr != nil {
				return fmt.Errorf("%w (rollback: %v)", err, rollbackErr)
			}
			return err
		}
		cloudNeeds = append(cloudNeeds, checkpointNeeds...)
		var restagedOutputs bool
		cloudNeeds, restagedOutputs, err = appendResumeCloudNeeds(opCtx, database, r2Client, job, cloudNeeds)
		if err != nil {
			rollbackErr := rollbackClaims()
			if rollbackErr != nil {
				return fmt.Errorf("%w (rollback: %v)", err, rollbackErr)
			}
			return err
		}
		agentJob.CloudNeeds = cloudNeeds
		agentJob.CloudAfter = cloudAfter
		agentJob.RestagedOutputs = restagedOutputs
		payload.Jobs = append(payload.Jobs, agentJob)
		payload.Sources = append(payload.Sources, controlplane.SourceUpdate{
			RemoteDir: remoteDir,
			R2Key:     sourceR2Key,
		})
	}

	inst, err = db.GetLaunch(database, instanceID)
	if err != nil {
		return fmt.Errorf("re-check instance %s: %w", ids.FormatInstanceID(instanceID), err)
	}
	if ok, reason := instanceAcceptsReuse(inst); !ok {
		return fmt.Errorf("instance %s cannot accept reused jobs: %s", ids.FormatInstanceID(instanceID), reason)
	}
	if ok, reason := instanceAcceptsReuseWithCachedLiveState(database, inst, time.Now()); !ok {
		rollbackErr := rollbackClaims()
		if rollbackErr != nil {
			return fmt.Errorf("instance %s cannot accept reused jobs: %s (rollback: %v)", ids.FormatInstanceID(instanceID), reason, rollbackErr)
		}
		return fmt.Errorf("instance %s cannot accept reused jobs: %s", ids.FormatInstanceID(instanceID), reason)
	}

	// Running instances drain jobs requests on a background interval while a
	// job executes, but the ack still arrives asynchronously. Write the
	// request to R2 without waiting for the ack; sync's
	// reconcilePendingMoveTargetRequestAcks consumes it. Grace instances poll
	// continuously, so we wait for the ack synchronously.
	targetRequestID := ""
	usedNoAckSubmission := false
	if inst.Status == db.LaunchStatusRunning {
		usedNoAckSubmission = true
		targetRequestID, err = sendGraceJobPayloadNoAck(opCtx, r2Client, instanceID, controlplane.GraceJobsRequest(payload))
		if err != nil {
			rollbackErr := rollbackClaims()
			if rollbackErr != nil {
				return fmt.Errorf("submit jobs to instance control plane: %w (rollback: %v)", err, rollbackErr)
			}
			return fmt.Errorf("submit jobs to instance control plane: %w", err)
		}
		if transfer {
			for _, claim := range moveClaims {
				if err := db.SetMoveIntentTargetRequest(database, claim.intentID, string(controlplane.GraceCommandJobs), targetRequestID); err != nil {
					rollbackErr := rollbackClaims()
					if rollbackErr != nil {
						return fmt.Errorf("record move target request for intent %d: %w (rollback: %v)", claim.intentID, err, rollbackErr)
					}
					return fmt.Errorf("record move target request for intent %d: %w", claim.intentID, err)
				}
			}
		}
	} else {
		if _, err := sendGraceJobPayload(opCtx, r2Client, instanceID, controlplane.GraceJobsRequest(payload)); err != nil {
			rollbackErr := rollbackClaims()
			if rollbackErr != nil {
				return fmt.Errorf("submit jobs to instance control plane: %w (rollback: %v)", err, rollbackErr)
			}
			return fmt.Errorf("submit jobs to instance control plane: %w", err)
		}
	}

	inst, err = db.GetLaunch(database, instanceID)
	if err != nil {
		return fmt.Errorf("final re-check instance %s: %w", ids.FormatInstanceID(instanceID), err)
	}
	if ok, reason := instanceAcceptsReuse(inst); !ok {
		return fmt.Errorf("instance %s cannot accept reused jobs: %s", ids.FormatInstanceID(instanceID), reason)
	}
	if ok, reason := instanceAcceptsReuseWithCachedLiveState(database, inst, time.Now()); !ok {
		return fmt.Errorf("instance %s cannot accept reused jobs: %s", ids.FormatInstanceID(instanceID), reason)
	}

	if transfer && usedNoAckSubmission {
		return nil
	}

	if transfer {
		for _, claim := range moveClaims {
			if err := db.ConfirmMoveTargetAccepted(database, claim.intentID, fmt.Sprintf("submitted to instance %s", ids.FormatInstanceID(instanceID))); err != nil {
				slog.Warn("confirm move target accepted", "component", "reuse", "intent_id", claim.intentID, "instance", instanceID, "error", err)
			}
			reset, err := ResetStrandedCloudAfterConsumers(database, claim.jobID, claim.sourceLaunchID)
			if err != nil {
				slog.Warn("reset stranded cloud-after consumers",
					"component", "reuse", "job_id", claim.jobID, "source_launch_id", claim.sourceLaunchID, "error", err)
			} else if len(reset.AttemptIDs) > 0 {
				cancelByLaunch[claim.sourceLaunchID] = append(cancelByLaunch[claim.sourceLaunchID], reset.AttemptIDs...)
				slog.Info("reset stranded cloud-after consumers",
					"component", "reuse", "job_id", claim.jobID, "source_launch_id", claim.sourceLaunchID, "consumer_job_ids", reset.JobIDs)
			}
		}
	}
	if r2Client != nil && len(cancelByLaunch) > 0 {
		for launchID, ids := range cancelByLaunch {
			if err := sendGraceCancelAttempts(opCtx, r2Client, launchID, ids); err != nil {
				slog.Warn("send cancel-attempts marker",
					"component", "reuse", "source_launch_id", launchID, "attempt_ids", ids, "error", err)
			}
		}
	}
	return nil
}

func validateJobsFitReusableInstance(database *sql.DB, inst *db.Launch, jobs []*db.Job, r2Client *r2.Client) error {
	if len(jobs) == 0 {
		return nil
	}
	cap, ok, err := newInstanceCapacityFromDB(database, inst, 0)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("instance %s cannot accept reused jobs", ids.FormatInstanceID(inst.ID))
	}
	for _, job := range jobs {
		if job == nil {
			continue
		}
		ok, reason := MatchJobToInstanceWithUV(job, cap, r2Client)
		if !ok {
			return fmt.Errorf("instance %s cannot accept job %s: %s", ids.FormatInstanceID(inst.ID), ids.FormatJobID(job.ID), reason)
		}
		consumeReuseJob(&cap, job)
	}
	return nil
}

func instanceAcceptsReuseWithCachedLiveState(database *sql.DB, inst *db.Launch, now time.Time) (bool, string) {
	live, err := db.GetLaunchLiveState(database, inst.ID)
	if err != nil {
		return false, fmt.Sprintf("read cached live state: %v", err)
	}
	return instanceAcceptsReuseWithLiveState(inst, live, now)
}

func resetClaimedJobsToUnplaced(database *sql.DB, jobIDs []int64) error {
	var rollbackErr error
	for _, jobID := range jobIDs {
		if err := db.ResetJobToUnplaced(database, jobID); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("reset job %s to unplaced: %w", ids.FormatJobID(jobID), err))
		}
	}
	return rollbackErr
}
