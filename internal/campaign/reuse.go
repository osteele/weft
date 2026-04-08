package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2"
	weftsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/workdir"
)

var (
	sendGraceJobPayload = controlplane.SendGraceJobPayload
	uploadSourceToR2    = weftsync.UploadSourceToR2
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

// FindReusableInstances returns non-terminal cloud instances that could accept new jobs.
// Returns instances with status "grace" or "running".
func FindReusableInstances(database *sql.DB) ([]InstanceCapacity, error) {
	instances, err := db.ListRunningLaunches(database)
	if err != nil {
		return nil, err
	}

	jobCounts, _ := db.GetActiveLaunchJobCounts(database)

	var result []InstanceCapacity
	for _, inst := range instances {
		if cap, ok := NewInstanceCapacity(inst, jobCounts[inst.ID]); ok {
			result = append(result, cap)
		}
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

// estimateInputsDisk estimates disk usage for a set of input refs in GB.
func estimateInputsDisk(inputs []string) int {
	if len(inputs) == 0 {
		return 0
	}
	totalBytes, err := dataloc.ResolveInputSizes(inputs, nil)
	if err != nil || totalBytes == 0 {
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

	// GPU class check (case-insensitive)
	if job.GPUClass != "" && !strings.EqualFold(job.GPUClass, inst.GPUClass) {
		// Also check resolved GPU name
		if !strings.EqualFold(job.GPUClass, inst.ResolvedGPUName) {
			return false, fmt.Sprintf("GPU class mismatch: job=%s instance=%s", job.GPUClass, inst.GPUClass)
		}
	}

	// GPU memory check
	jobMemGB := 0
	if job.GPUMemGB != nil {
		jobMemGB = *job.GPUMemGB
	}
	if jobMemGB > 0 && jobMemGB > inst.GPUMemGB {
		return false, fmt.Sprintf("GPU memory insufficient: job=%dGB instance=%dGB", jobMemGB, inst.GPUMemGB)
	}

	// Disk check: compute incremental inputs (HF models) + UV sync
	if cap.DiskFreeGB > 0 || inst.DiskGB > 0 {
		incrementalInputs := subtractInputs(job.Inputs, cap.ProvisionedInputs)
		incrementalDiskGB := estimateInputsDisk(incrementalInputs)

		// Account for cold uv sync disk from the local cache when available,
		// and fall back to R2 when a client is provided.
		localDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
		if localDir != "" {
			uvBytes := estimateGroupUVBytes([]string{localDir}, r2Client)
			incrementalDiskGB += int(math.Ceil(float64(uvBytes) / 1e9))
		}

		if incrementalDiskGB > 0 && incrementalDiskGB > cap.DiskFreeGB {
			return false, fmt.Sprintf("disk insufficient: need=%dGB free=%dGB", incrementalDiskGB, cap.DiskFreeGB)
		}
	}

	// Grace deadline check
	if cap.GraceRemaining > 0 && cap.GraceRemaining < MinGraceRemaining {
		return false, fmt.Sprintf("grace period too short: %s remaining", cap.GraceRemaining.Truncate(time.Second))
	}

	return true, ""
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
// Mutates the instances slice to track consumed capacity.
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

	cap.DiskFreeGB -= estimateInputsDisk(incremental)
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
			detail = fmt.Sprintf("running — %d job(s) ahead", a.Instance.RunningJobCount-1)
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

// GracePayload is the JSON structure written to R2 for job submission.
type GracePayload struct {
	Jobs    []cloud.AgentJob            `json:"jobs"`
	Sources []controlplane.SourceUpdate `json:"sources,omitempty"`
}

// SubmitJobsToInstance submits one or more jobs to an existing cloud instance
// via R2 grace protocol. Works for both grace and running instances.
func SubmitJobsToInstance(ctx context.Context, database *sql.DB, r2Client *r2.Client, instanceID int64, jobs []*db.Job) error {
	inst, err := db.GetLaunch(database, instanceID)
	if err != nil {
		return fmt.Errorf("get instance %s: %w", ids.FormatInstanceID(instanceID), err)
	}
	if ok, reason := instanceAcceptsReuse(inst); !ok {
		return fmt.Errorf("instance %s cannot accept reused jobs: %s", ids.FormatInstanceID(instanceID), reason)
	}

	payload := GracePayload{
		Sources: []controlplane.SourceUpdate{},
	}

	// Sort jobs by ID so the agent executes them in submission order
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].ID < jobs[j].ID
	})

	// Upload sources and build payload
	for _, job := range jobs {
		sourceDir := workdir.ResolveLocal(job.EffectiveWorkingDir())

		// Upload fresh sources (content-addressed, so deduped)
		sourceR2Key, err := uploadSourceToR2(ctx, r2Client, sourceDir)
		if err != nil {
			return fmt.Errorf("upload source for job %d: %w", job.ID, err)
		}

		// Compute remote working directory under the synced project root.
		remoteDir := path.Join(cloud.ProjectRootDir, path.Base(sourceDir))

		payload.Jobs = append(payload.Jobs, newAgentJob(job, remoteDir))
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

	if _, err := sendGraceJobPayload(ctx, r2Client, instanceID, controlplane.GraceJobsRequest(payload)); err != nil {
		return fmt.Errorf("submit jobs to instance control plane: %w", err)
	}

	inst, err = db.GetLaunch(database, instanceID)
	if err != nil {
		return fmt.Errorf("final re-check instance %s: %w", ids.FormatInstanceID(instanceID), err)
	}
	if ok, reason := instanceAcceptsReuse(inst); !ok {
		return fmt.Errorf("instance %s cannot accept reused jobs: %s", ids.FormatInstanceID(instanceID), reason)
	}

	for _, job := range jobs {
		if err := db.SetJobLaunchID(database, job.ID, instanceID); err != nil {
			return fmt.Errorf("associate job %d with instance %s: %w", job.ID, ids.FormatInstanceID(instanceID), err)
		}
	}

	return nil
}
