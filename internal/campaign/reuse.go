package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	weftsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/workdir"
)

// MinGraceRemaining is the minimum grace period remaining to consider an
// instance for reuse. Instances with less time are auto-extended.
const MinGraceRemaining = 5 * time.Minute

// InstanceCapacity describes a reusable cloud instance and its available resources.
type InstanceCapacity struct {
	Instance          *db.CloudInstance
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

// FindReusableInstances returns non-terminal cloud instances that could accept new jobs.
// Returns instances with status "grace" or "running".
func FindReusableInstances(database *sql.DB) ([]InstanceCapacity, error) {
	instances, err := db.ListRunningCloudInstances(database)
	if err != nil {
		return nil, err
	}

	jobCounts, _ := db.GetCloudInstanceJobCounts(database)

	var result []InstanceCapacity
	for _, inst := range instances {
		if inst.Status != db.CloudInstanceStatusGrace && inst.Status != db.CloudInstanceStatusRunning {
			continue // skip "launching"
		}

		cap := InstanceCapacity{
			Instance:          inst,
			ProvisionedInputs: inst.ProvisionedInputs,
			RunningJobCount:   jobCounts[inst.ID],
		}

		// Estimate free disk
		cap.DiskFreeGB = estimateDiskFree(inst)

		// Compute grace remaining
		if inst.Status == db.CloudInstanceStatusGrace && inst.GraceDeadline != nil {
			cap.GraceRemaining = time.Until(time.Unix(*inst.GraceDeadline, 0))
			if cap.GraceRemaining <= 0 {
				continue // expired, skip
			}
		}

		result = append(result, cap)
	}

	return result, nil
}

// estimateDiskFree estimates the free disk space on an instance.
func estimateDiskFree(inst *db.CloudInstance) int {
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

	// Disk check: compute incremental inputs needed
	if cap.DiskFreeGB > 0 || inst.DiskGB > 0 {
		incrementalInputs := subtractInputs(job.Inputs, cap.ProvisionedInputs)
		incrementalDiskGB := estimateInputsDisk(incrementalInputs)
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
	aGrace := a.Instance.Status == db.CloudInstanceStatusGrace
	bGrace := b.Instance.Status == db.CloudInstanceStatusGrace

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

		// Mark instance as consumed: update provisioned inputs and job count
		for i := range instances {
			if instances[i].Instance.ID == best.Instance.ID {
				// Add job's inputs to provisioned
				for _, input := range job.Inputs {
					if !slices.Contains(instances[i].ProvisionedInputs, input) {
						instances[i].ProvisionedInputs = append(instances[i].ProvisionedInputs, input)
					}
				}
				// Reduce free disk by incremental inputs
				incremental := subtractInputs(job.Inputs, best.ProvisionedInputs)
				instances[i].DiskFreeGB -= estimateInputsDisk(incremental)
				if instances[i].DiskFreeGB < 0 {
					instances[i].DiskFreeGB = 0
				}
				instances[i].RunningJobCount++
				break
			}
		}
	}

	return assignments, remaining
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
		if inst.Status == db.CloudInstanceStatusGrace {
			remaining := a.Instance.GraceRemaining.Truncate(time.Second)
			detail = fmt.Sprintf("grace — %s remaining", remaining)
		} else {
			detail = fmt.Sprintf("running — %d job(s) ahead", a.Instance.RunningJobCount-1)
		}
		cost := "$0.00"
		if inst.Status == db.CloudInstanceStatusRunning {
			cost = "$0.00 (shared)"
		}
		b.WriteString(fmt.Sprintf("  Job #%d  →  Instance #%d (%s, %s)  %s\n",
			a.Job.ID, inst.ID, inst.DisplayGPUSpec(), detail, cost))
	}
	return b.String()
}

// GracePayload is the JSON structure written to R2 for job submission.
type GracePayload struct {
	Jobs    []cloud.AgentJob  `json:"jobs"`
	Sources map[string]string `json:"sources"`
}

// SubmitJobsToInstance submits one or more jobs to an existing cloud instance
// via R2 grace protocol. Works for both grace and running instances.
func SubmitJobsToInstance(ctx context.Context, database *sql.DB, r2Client *r2.Client, instanceID int64, jobs []*db.Job) error {
	payload := GracePayload{
		Sources: make(map[string]string),
	}

	// Upload sources and build payload
	for _, job := range jobs {
		sourceDir := workdir.ResolveLocal(job.EffectiveWorkingDir())

		// Upload fresh sources (content-addressed, so deduped)
		sourceR2Key, err := weftsync.UploadSourceToR2(ctx, r2Client, sourceDir)
		if err != nil {
			return fmt.Errorf("upload source for job %d: %w", job.ID, err)
		}

		payload.Jobs = append(payload.Jobs, cloud.AgentJob{
			ID:      job.ID,
			Command: job.EffectiveCommand(),
		})
		payload.Sources[sourceDir] = sourceR2Key
	}

	// Write jobs.json to R2
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	graceKey := r2keys.GraceJobs(instanceID)
	if err := r2Client.PutObject(ctx, graceKey, strings.NewReader(string(payloadJSON)), "application/json"); err != nil {
		return fmt.Errorf("write jobs.json to R2: %w", err)
	}

	// Associate jobs with the cloud instance
	for _, job := range jobs {
		if err := db.SetJobCloudInstanceID(database, job.ID, instanceID); err != nil {
			return fmt.Errorf("associate job %d with instance %d: %w", job.ID, instanceID, err)
		}
	}

	return nil
}
