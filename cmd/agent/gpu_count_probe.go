package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2keys"
)

// phaseGPUCountInsufficient is the lifecycle phase string for the infra
// failure raised when the instance exposes fewer GPUs than its jobs request.
const phaseGPUCountInsufficient = "infra-failure:gpu-count-insufficient"

type gpuCountFailureReport struct {
	TimestampUnix   int64  `json:"timestamp_unix"`
	RequestedGPUs   int    `json:"requested_gpus"`
	VisibleGPUs     int    `json:"visible_gpus"`
	NvidiaSmiOutput string `json:"nvidia_smi_query_output,omitempty"`
}

// maxRequestedGPUCount returns the largest per-job GPU count across the
// sequence. Jobs that requested 0 or 1 GPU never fail the shape check.
func maxRequestedGPUCount(jobs []cloud.AgentJob) int {
	maxCount := 0
	for _, job := range jobs {
		if job.GPUCount > maxCount {
			maxCount = job.GPUCount
		}
	}
	return maxCount
}

// checkGPUCount verifies the instance physically exposes at least
// requiredCount GPUs BEFORE any billable setup / HF prewarm work runs.
// Placement filters offers by GPU count (campaign.filterOffersByGPUCount)
// and the runner re-checks availability just before the user command
// (gpu_count_preflight_failed), but a provider that allocated fewer devices
// than the offer advertised was previously only discovered AFTER prewarm
// spend (wb41). On a shortfall it uploads a structured failure report to R2
// and self-destructs the instance with infra_failure so the reconciler
// requeues the jobs onto a correctly shaped instance.
//
// Returns true if the instance was terminated. Mirrors checkDriverVersion's
// fail-open posture when nvidia-smi is unavailable or unparseable: failing
// closed on broken probe tooling would strand CPU-only instances.
func checkGPUCount(r2Bucket string, instanceID int64, requiredCount int, selfDestructCmd string) bool {
	if requiredCount <= 1 {
		return false
	}
	out, err := commandOutput(5*time.Second, "nvidia-smi",
		"--query-gpu=index", "--format=csv,noheader")
	if err != nil {
		fmt.Fprintf(os.Stderr, "gpu count probe: nvidia-smi failed: %v (skipping check)\n", err)
		return false
	}
	visible := parseGPUCount(out)
	if visible < 0 {
		fmt.Fprintf(os.Stderr, "gpu count probe: cannot parse nvidia-smi output %q (skipping check)\n", out)
		return false
	}
	if visible >= requiredCount {
		fmt.Printf("gpu count probe: ok — required %d, visible %d\n", requiredCount, visible)
		return false
	}

	report := gpuCountFailureReport{
		TimestampUnix:   time.Now().Unix(),
		RequestedGPUs:   requiredCount,
		VisibleGPUs:     visible,
		NvidiaSmiOutput: out,
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	_ = r2Put(r2Bucket, r2keys.InstanceGPUCountFailure(instanceID), string(data))

	msg := fmt.Sprintf("gpu_count_preflight_failed: requested=%d visible=%d; terminating before setup/prewarm spend",
		requiredCount, visible)
	fmt.Fprintln(os.Stderr, "gpu count probe: "+msg)
	oplog.Log(oplog.OpPhaseTransition, oplog.WithDetail(phaseGPUCountInsufficient))

	// The infrastructure-vs-user decision for a GPU-count shortfall is owned
	// by db.ClassifyInfraFailure (PhaseGPUCountPreflight), the same
	// classifier the prewarm, setup, and runtime paths funnel through.
	if _, infra := db.ClassifyInfraFailure(db.PhaseGPUCountPreflight, 0, ""); infra {
		terminateInstanceWithReason(r2Bucket, instanceID, selfDestructCmd, phaseGPUCountInsufficient, 0,
			db.TerminationReasonInfraFailure, msg)
	}
	return true
}

// parseGPUCount counts the device rows in `nvidia-smi --query-gpu=index
// --format=csv,noheader` output. Returns -1 when the output contains
// non-numeric noise (NVML errors, driver mismatch banners) so callers skip
// the check rather than treating garbage as "zero GPUs".
func parseGPUCount(out string) int {
	count := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, r := range line {
			if r < '0' || r > '9' {
				return -1
			}
		}
		count++
	}
	return count
}
