package cmd

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/remediation"
)

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
