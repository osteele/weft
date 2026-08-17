package cmd

import (
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
			return dropUnsupportedTimeout(job, d)
		}
	}
	if job.Status == db.StatusCompleted && job.ExitCode != nil && *job.ExitCode == 0 &&
		strings.TrimSpace(job.FailureReason) == "" && strings.TrimSpace(job.ErrorMessage) == "" {
		return nil
	}
	cached, err := logcache.Read(job.ID)
	if err != nil || cached == "" {
		return nil
	}
	return dropUnsupportedTimeout(job, remediation.DiagnoseFromLog(cached))
}

// timeoutExitCodes are the exit statuses a killed-on-deadline process
// actually carries: SIGTERM and SIGKILL as reported by a shell, plus
// coreutils timeout(1).
var timeoutExitCodes = map[int]bool{124: true, 137: true, 143: true}

// dropUnsupportedTimeout suppresses a timeout diagnosis for a job that
// exited under its own control. The timeout rule matches a bare "timed out"
// or "timeout" token anywhere in the log, which a nested traceback or a
// harness line can supply on its own; the exit status is the independent
// check. A deliberate non-zero exit — a pre-registered gate reporting its
// verdict, say — is a result, and calling it a timeout invites a pointless
// re-run on a longer cap (wb68).
func dropUnsupportedTimeout(job *db.Job, d *remediation.ErrorDiagnosis) *remediation.ErrorDiagnosis {
	if d == nil || d.Pattern != "timeout" {
		return d
	}
	if job.ExitCode == nil || *job.ExitCode == 0 || timeoutExitCodes[*job.ExitCode] {
		return d
	}
	return nil
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

	detail := d.GPUOOMDetailLines()
	if len(detail) == 0 {
		return msg
	}

	parts := append([]string{msg}, detail...)
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
