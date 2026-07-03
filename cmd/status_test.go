package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/remediation"
)

func TestLogContentHasExitMarker(t *testing.T) {
	if !logContentHasExitMarker("some output\n=== END exit=1 (crashed) Thu ===\n") {
		t.Fatalf("expected exit marker to be detected")
	}
	if logContentHasExitMarker("still running\nProgress: 42%\n") {
		t.Fatalf("did not expect an exit marker in running output")
	}
}

func TestMaybeWarnStaleRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const jobID int64 = 4242
	staleStart := time.Now().Unix() - int64(2*staleRunningExitHintAge.Seconds())
	endLog := "output line\n=== END exit=1 (crashed) Thu Jul  3 ===\n"

	runningWithExit := func() *db.Job {
		// A placed host keeps EffectiveStatus() == running (an unplaced running
		// job is reported as queued).
		return &db.Job{ID: jobID, Host: "host-alpha", Status: db.StatusRunning, StartTime: staleStart}
	}

	t.Run("fires for stale running job whose log exited", func(t *testing.T) {
		if err := logcache.Write(jobID, endLog); err != nil {
			t.Fatalf("write cache: %v", err)
		}
		var buf bytes.Buffer
		maybeWarnStaleRunning(&buf, runningWithExit())
		if !strings.Contains(buf.String(), "log shows it exited") {
			t.Fatalf("expected stale-running hint, got %q", buf.String())
		}
		if !strings.Contains(buf.String(), ids.FormatJobID(jobID)) {
			t.Fatalf("expected hint to reference the job id, got %q", buf.String())
		}
	})

	t.Run("silent for recently running job", func(t *testing.T) {
		if err := logcache.Write(jobID, endLog); err != nil {
			t.Fatalf("write cache: %v", err)
		}
		job := runningWithExit()
		job.StartTime = time.Now().Unix()
		var buf bytes.Buffer
		maybeWarnStaleRunning(&buf, job)
		if buf.Len() != 0 {
			t.Fatalf("expected no hint for a just-started job, got %q", buf.String())
		}
	})

	t.Run("silent for terminal job", func(t *testing.T) {
		if err := logcache.Write(jobID, endLog); err != nil {
			t.Fatalf("write cache: %v", err)
		}
		job := runningWithExit()
		job.Status = db.StatusCompleted
		var buf bytes.Buffer
		maybeWarnStaleRunning(&buf, job)
		if buf.Len() != 0 {
			t.Fatalf("expected no hint for a terminal job, got %q", buf.String())
		}
	})

	t.Run("silent when log has no exit marker", func(t *testing.T) {
		if err := logcache.Write(jobID, "still going\nProgress: 3/10\n"); err != nil {
			t.Fatalf("write cache: %v", err)
		}
		var buf bytes.Buffer
		maybeWarnStaleRunning(&buf, runningWithExit())
		if buf.Len() != 0 {
			t.Fatalf("expected no hint when log lacks an exit marker, got %q", buf.String())
		}
	})

	t.Run("silent when cached log is a different attempt", func(t *testing.T) {
		if err := logcache.WriteForRun(jobID, 5, endLog); err != nil {
			t.Fatalf("write cache: %v", err)
		}
		job := runningWithExit()
		otherRun := int64(6)
		job.LatestRunID = &otherRun
		var buf bytes.Buffer
		maybeWarnStaleRunning(&buf, job)
		if buf.Len() != 0 {
			t.Fatalf("expected no hint when cache is from another attempt, got %q", buf.String())
		}
	})
}

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
