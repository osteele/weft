package cmd

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/remediation"
)

// ResolveJobDiagnosis falls back to a live log-cache scan when the
// stored diagnosis is empty so jobs that never invoked the remediation
// pipeline (benchmark / processed / no-retry failures) still surface a
// pattern. See specs/job-lifecycle.allium.
func ResolveJobDiagnosis(job *db.Job) *remediation.ErrorDiagnosis {
	if job == nil {
		return nil
	}
	if job.ErrorDiagnosis != "" {
		if d, err := remediation.UnmarshalDiagnosis(job.ErrorDiagnosis); err == nil && d != nil {
			return d
		}
	}
	cached, err := logcache.Read(job.ID)
	if err != nil || cached == "" {
		return nil
	}
	return remediation.DiagnoseFromLog(cached)
}

func formatDiagnosisSummary(d *remediation.ErrorDiagnosis) string {
	if d == nil {
		return ""
	}
	msg := strings.TrimSpace(d.Message)
	if d.Pattern != "gpu_oom" {
		if d.Solution != "" && msg != "" {
			return msg + "; solution: " + strings.TrimSpace(d.Solution)
		}
		return msg
	}

	if len(d.GPUOOMProcesses) == 0 {
		return msg
	}

	mainPID := d.GPUOOMMainPID
	mainGiB := d.GPUOOMProcesses[0].MemoryGiB
	if mainPID == 0 {
		mainPID = d.GPUOOMProcesses[0].PID
	}

	parts := []string{
		msg,
		fmt.Sprintf("main GPU process pid=%d using %.2f GiB", mainPID, mainGiB),
	}

	if d.GPUOOMExtraPID > 0 && d.GPUOOMExtraGiB > 0 {
		parts = append(parts, fmt.Sprintf("additional GPU process pid=%d using %.2f GiB", d.GPUOOMExtraPID, d.GPUOOMExtraGiB))
	}
	if d.GPUOOMNotes != "" {
		parts = append(parts, d.GPUOOMNotes)
	}
	if d.GPUOOMHintDeltaGB > 0 {
		parts = append(parts, fmt.Sprintf("hint: increase --gpu-mem by ~%dGB on retry", d.GPUOOMHintDeltaGB))
	}
	if d.Solution != "" {
		parts = append(parts, "solution: "+strings.TrimSpace(d.Solution))
	}

	return strings.Join(parts, "; ")
}

func diagnosisSummaryFromJSON(payload string) string {
	if payload == "" {
		return ""
	}
	d, err := remediation.UnmarshalDiagnosis(payload)
	if err != nil {
		return ""
	}
	return formatDiagnosisSummary(d)
}
