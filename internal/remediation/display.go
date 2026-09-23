package remediation

import (
	"strings"

	"github.com/osteele/weft/internal/db"
)

// ResolveDisplayDiagnosis returns the diagnosis weft displays for job: the
// stored diagnosis when one parses, otherwise a scan of the job's cached log.
// readLog supplies that log and is called only when the fallback is needed; an
// error or empty log yields no diagnosis. Both results pass through
// DropUnsupportedTimeout. See specs/job-lifecycle.allium.
func ResolveDisplayDiagnosis(job *db.Job, readLog func() (string, error)) *ErrorDiagnosis {
	if job == nil {
		return nil
	}
	if job.ErrorDiagnosis != "" {
		if d, err := UnmarshalDiagnosis(job.ErrorDiagnosis); err == nil && d != nil {
			return DropUnsupportedTimeout(job, d)
		}
	}
	if job.Status == db.StatusCompleted && job.ExitCode != nil && *job.ExitCode == 0 &&
		strings.TrimSpace(job.FailureReason) == "" && strings.TrimSpace(job.ErrorMessage) == "" {
		return nil
	}
	cached, err := readLog()
	if err != nil || cached == "" {
		return nil
	}
	return DropUnsupportedTimeout(job, DiagnoseFromLog(cached))
}

// timeoutExitCodes are the exit statuses a killed-on-deadline process
// actually carries: SIGTERM and SIGKILL as reported by a shell, plus
// coreutils timeout(1).
var timeoutExitCodes = map[int]bool{124: true, 137: true, 143: true}

// DropUnsupportedTimeout suppresses a timeout diagnosis for a job that
// exited under its own control. The timeout rule matches a bare "timed out"
// or "timeout" token anywhere in the log, which a nested traceback or a
// harness line can supply on its own; the exit status is the independent
// check. A deliberate non-zero exit — a pre-registered gate reporting its
// verdict, say — is a result, and calling it a timeout invites a pointless
// re-run on a longer cap (wb68).
func DropUnsupportedTimeout(job *db.Job, d *ErrorDiagnosis) *ErrorDiagnosis {
	if d == nil || d.Pattern != "timeout" {
		return d
	}
	if job.ExitCode == nil || *job.ExitCode == 0 || timeoutExitCodes[*job.ExitCode] {
		return d
	}
	return nil
}
