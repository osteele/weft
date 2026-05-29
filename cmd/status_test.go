package cmd

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/remediation"
)

func TestIsWaitTerminalStatus(t *testing.T) {
	waitTerminal := []string{
		db.StatusCompleted,
		db.StatusDead,
		db.StatusFailed,
		db.StatusKilled,
		db.StatusCanceled,
	}
	for _, s := range waitTerminal {
		if !isWaitTerminalStatus(s) {
			t.Errorf("isWaitTerminalStatus(%q) = false, want true", s)
		}
	}

	// Draft is terminal but NOT wait-terminal
	if isWaitTerminalStatus(db.StatusDraft) {
		t.Error("isWaitTerminalStatus(draft) = true, want false")
	}

	nonTerminal := []string{
		db.StatusRunning,
		db.StatusStarting,
		db.StatusQueued,
	}
	for _, s := range nonTerminal {
		if isWaitTerminalStatus(s) {
			t.Errorf("isWaitTerminalStatus(%q) = true, want false", s)
		}
	}
}

func TestShouldAttemptSync(t *testing.T) {
	syncable := []string{
		db.StatusRunning,
		db.StatusStarting,
		db.StatusPaused,
		db.StatusQueued,
	}
	for _, s := range syncable {
		if !shouldAttemptSync(s) {
			t.Errorf("shouldAttemptSync(%q) = false, want true", s)
		}
	}

	nonSyncable := []string{
		db.StatusCompleted,
		db.StatusDead,
		db.StatusFailed,
		db.StatusKilled,
		db.StatusDraft,
	}
	for _, s := range nonSyncable {
		if shouldAttemptSync(s) {
			t.Errorf("shouldAttemptSync(%q) = true, want false", s)
		}
	}
}

func TestFormatJobIDList(t *testing.T) {
	tests := []struct {
		name string
		ids  []int64
		want string
	}{
		{"sorts descending to ascending", []int64{3, 1, 2}, "wj1:wj3"},
		{"empty slice", []int64{}, ""},
		{"single element", []int64{42}, "wj42"},
		{"already sorted", []int64{10, 20, 30}, "wj10,wj20,wj30"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ids.FormatJobIDListCompact(tt.ids)
			if got != tt.want {
				t.Errorf("FormatJobIDListCompact(%v) = %q, want %q", tt.ids, got, tt.want)
			}
		})
	}
}

func TestPrintDiagnosisSummary_GPUOOMMultiLine(t *testing.T) {
	job := &db.Job{
		ErrorDiagnosis: mustMarshalDiagnosis(t, &remediation.ErrorDiagnosis{
			Pattern:           "gpu_oom",
			Message:           "GPU out of memory",
			Solution:          "Reduce batch size or request more GPU memory",
			GPUOOMMainPID:     12345,
			GPUOOMProcesses:   []remediation.GPUOOMProcess{{PID: 12345, MemoryGiB: 78.42}},
			GPUOOMExtraPID:    67890,
			GPUOOMExtraGiB:    2.10,
			GPUOOMHintDeltaGB: 8,
			GPUOOMNotes:       "shared workspace VRAM contention",
		}),
	}

	out := captureStdout(t, func() {
		printDiagnosisSummary(job)
	})

	for _, want := range []string{
		"Diagnosis: GPU out of memory (gpu_oom)",
		"Solution:  Reduce batch size or request more GPU memory",
		"           main GPU process pid=12345 using 78.42 GiB",
		"           additional GPU process pid=67890 using 2.10 GiB",
		"           shared workspace VRAM contention",
		"           hint: increase --gpu-mem by ~8GB on retry",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q in:\n%s", want, out)
		}
	}

	mainIdx := strings.Index(out, "main GPU process")
	solIdx := strings.Index(out, "Solution:")
	if mainIdx < 0 || solIdx < 0 || mainIdx < solIdx {
		t.Errorf("expected gpu_oom detail after Solution, got:\n%s", out)
	}
}

func TestPrintDiagnosisSummary_NonGPUOOM_NoExtraLines(t *testing.T) {
	job := &db.Job{
		ErrorDiagnosis: mustMarshalDiagnosis(t, &remediation.ErrorDiagnosis{
			Pattern: "module_not_found",
			Message: "missing module: torch",
		}),
	}

	out := captureStdout(t, func() {
		printDiagnosisSummary(job)
	})

	if strings.Contains(out, "main GPU process") || strings.Contains(out, "hint: increase --gpu-mem") {
		t.Errorf("non-gpu_oom diagnosis surfaced gpu_oom detail, got:\n%s", out)
	}
}
