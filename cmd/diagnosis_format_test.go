package cmd

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/remediation"
)

func TestFormatDiagnosisSummary_GPUOOMAttribution(t *testing.T) {
	d := &remediation.ErrorDiagnosis{
		Pattern:  "gpu_oom",
		Category: "environment",
		Message:  "GPU out of memory",
		GPUOOMProcesses: []remediation.GPUOOMProcess{
			{PID: 1886134, MemoryGiB: 9.18},
			{PID: 1887281, MemoryGiB: 2.53},
		},
		GPUOOMMainPID:     1886134,
		GPUOOMExtraPID:    1887281,
		GPUOOMExtraGiB:    2.53,
		GPUOOMHintDeltaGB: 4,
		GPUOOMNotes:       "PIDs are container-local; identical PID values can appear across different containers.",
	}

	got := formatDiagnosisSummary(d)
	for _, needle := range []string{
		"GPU out of memory",
		"main GPU process pid=1886134 using 9.18 GiB",
		"additional GPU process pid=1887281 using 2.53 GiB",
		"increase --gpu-mem by ~4GB on retry",
	} {
		if !strings.Contains(got, needle) {
			t.Fatalf("formatDiagnosisSummary missing %q in %q", needle, got)
		}
	}
}

func TestDiagnosisSummaryFromJSON_InvalidPayload(t *testing.T) {
	if got := diagnosisSummaryFromJSON("{invalid"); got != "" {
		t.Fatalf("diagnosisSummaryFromJSON(invalid) = %q, want empty", got)
	}
}
