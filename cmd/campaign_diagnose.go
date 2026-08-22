package cmd

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/spf13/cobra"
)

var campaignDiagnoseCmd = &cobra.Command{
	Use:   "diagnose [campaign-id]",
	Short: "Explain why campaign instances failed",
	Long: `Summarizes why cloud instances in a campaign failed.

If no campaign ID is provided, diagnoses the most recent failed campaign.`,
	Args: usageArgs(cobra.MaximumNArgs(1)),
	RunE: runCampaignDiagnose,
}

type campaignDiagnosisReport struct {
	Campaign  *db.Campaign
	Instances []instanceDiagnosis
}

type instanceDiagnosis struct {
	Instance    *db.Launch
	Summary     string
	JobFindings []jobFinding
}

type jobFinding struct {
	JobID   int64
	Summary string
}

func runCampaignDiagnose(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	campaignID, err := resolveCampaignDiagnoseID(database, args)
	if err != nil {
		return err
	}

	report, err := buildCampaignDiagnosisReport(database, campaignID)
	if err != nil {
		return err
	}

	fmt.Print(formatCampaignDiagnosisReport(report))
	return nil
}

func resolveCampaignDiagnoseID(database *sql.DB, args []string) (int64, error) {
	if len(args) > 0 {
		return parseCampaignID(args[0])
	}

	c, err := db.GetMostRecentCampaignByStatus(database, db.CampaignStatusFailed)
	if err != nil {
		return 0, fmt.Errorf("get most recent failed campaign: %w", err)
	}
	if c == nil {
		return 0, fmt.Errorf("no failed campaigns found")
	}
	return c.ID, nil
}

func parseCampaignID(arg string) (int64, error) {
	campaignID, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return 0, usageErrorf("invalid campaign ID %q", arg)
	}
	return campaignID, nil
}

func buildCampaignDiagnosisReport(database *sql.DB, campaignID int64) (*campaignDiagnosisReport, error) {
	c, err := db.GetCampaign(database, campaignID)
	if err != nil {
		return nil, fmt.Errorf("get campaign: %w", err)
	}
	if c == nil {
		return nil, fmt.Errorf("campaign %d not found", campaignID)
	}

	instances, err := db.GetCampaignInstances(database, campaignID)
	if err != nil {
		return nil, fmt.Errorf("get campaign instances: %w", err)
	}
	if len(instances) == 0 {
		return nil, fmt.Errorf("campaign %d has no instances", campaignID)
	}

	report := &campaignDiagnosisReport{Campaign: c}
	for _, inst := range instances {
		if inst.TerminationReason == db.TerminationReasonJobFailure {
			_ = db.RefineInstanceTerminationReason(database, inst.ID)
			if refreshed, err := db.GetLaunch(database, inst.ID); err == nil && refreshed != nil {
				inst = refreshed
			}
		}

		jobs, err := db.GetLaunchJobsIncludingAttempts(database, inst.ID)
		if err != nil {
			return nil, fmt.Errorf("get jobs for instance %s: %w", ids.FormatInstanceID(inst.ID), err)
		}

		outcomes, err := db.GetAttemptOutcomesByLaunch(database, inst.ID)
		if err != nil {
			return nil, fmt.Errorf("get attempt outcomes for instance %s: %w", ids.FormatInstanceID(inst.ID), err)
		}

		finding := buildInstanceDiagnosis(inst, jobs, outcomes)
		report.Instances = append(report.Instances, finding)
	}

	return report, nil
}

func buildInstanceDiagnosis(inst *db.Launch, jobs []*db.Job, outcomes map[int64]string) instanceDiagnosis {
	result := instanceDiagnosis{Instance: inst}
	undeclaredInputCount := 0
	for _, job := range jobs {
		if summary := summarizeJobIssue(job, outcomes[job.ID]); summary != "" {
			result.JobFindings = append(result.JobFindings, jobFinding{
				JobID:   job.ID,
				Summary: summary,
			})
		}
		// For disk-full failures, report undeclared HF models
		if inst.TerminationReason == db.TerminationReasonDiskFull && len(job.ObservedInputs) > 0 {
			undeclaredInputCount += len(job.ObservedInputs)
			for _, input := range job.ObservedInputs {
				result.JobFindings = append(result.JobFindings, jobFinding{
					JobID:   job.ID,
					Summary: fmt.Sprintf("undeclared input %s — re-submit with --input %s", input, input),
				})
			}
		}
	}
	result.Summary = summarizeInstanceCause(inst, result.JobFindings, outcomes, undeclaredInputCount)
	return result
}

func summarizeInstanceCause(inst *db.Launch, findings []jobFinding, outcomes map[int64]string, undeclaredInputCount int) string {
	switch inst.TerminationReason {
	case db.TerminationReasonProviderFailure:
		return "provider terminated the instance"
	case cloud.ProviderStatusDestroyed, cloud.ProviderStatusDead, cloud.ProviderStatusStopped:
		return fmt.Sprintf("provider reported instance %s", inst.TerminationReason)
	case cloud.ProviderStatusError:
		return "provider reported instance error"
	case cloud.ProviderStatusExited:
		if len(findings) > 0 {
			return findings[0].Summary
		}
		return "container process exited"
	case db.TerminationReasonInfraFailure:
		if inst.TerminationDetail != "" {
			return inst.TerminationDetail
		}
		if countOutcome(outcomes, db.AttemptOutcomeOrphaned) > 0 {
			return "instance became unreachable or was terminated before jobs finished"
		}
		return "instance setup or infrastructure failed"
	case db.TerminationReasonDiskFull:
		if undeclaredInputCount > 0 {
			return fmt.Sprintf("instance disk filled — %d undeclared HF model(s) found in cache", undeclaredInputCount)
		}
		return "instance disk filled during execution"
	case db.TerminationReasonJobFailure:
		if len(findings) == 1 {
			return findings[0].Summary
		}
		if len(findings) > 1 {
			return "multiple jobs failed on the instance"
		}
		return "one or more jobs failed on the instance"
	case db.TerminationReasonCancelled:
		return "terminated by user"
	case db.TerminationReasonCompleted:
		if inst.ResultsVerified != nil && !*inst.ResultsVerified {
			switch inst.ResultsVerifyDetail {
			case db.ResultsVerifyDetailUploadsIncomplete:
				return "completed but one or more job uploads were partial/failed"
			case db.ResultsVerifyDetailManifestMissingJobs:
				return "completed but the completion manifest is missing results for some jobs"
			default:
				return "completed but results upload could not be confirmed"
			}
		}
		return "completed successfully"
	case db.TerminationReasonAccountCreditExhausted:
		return "provider account credit exhausted"
	}

	switch inst.Status {
	case db.LaunchStatusCompleted:
		return "completed successfully"
	case db.LaunchStatusCancelled:
		return "terminated by user"
	case db.LaunchStatusFailed:
		if countOutcome(outcomes, db.AttemptOutcomeOrphaned) > 0 {
			return "instance failed and orphaned in-flight jobs"
		}
		return "instance failed"
	case db.LaunchStatusGrace:
		return inst.GraceStatusLabel()
	case db.LaunchStatusRunning:
		return "instance is still running"
	case db.LaunchStatusLaunching:
		return "instance is still launching"
	default:
		return "instance has not reached a terminal state"
	}
}

func summarizeJobIssue(job *db.Job, outcome string) string {
	parts := make([]string, 0, 4)

	if outcome == db.AttemptOutcomeOrphaned {
		parts = appendUnique(parts, "orphaned after the instance terminated")
	}
	if msg := diagnosisMessageForJob(job); msg != "" {
		parts = appendUnique(parts, msg)
	}
	if reason := humanizeFailureReason(job.FailureReason); reason != "" {
		parts = appendUnique(parts, reason)
	}
	if len(parts) == 0 && job.ErrorMessage != "" {
		parts = appendUnique(parts, firstLine(job.ErrorMessage))
	}
	if len(parts) == 0 && job.ExitCode != nil && *job.ExitCode != 0 {
		parts = appendUnique(parts, fmt.Sprintf("exit %d", *job.ExitCode))
	}
	if len(parts) == 0 {
		switch outcome {
		case db.AttemptOutcomeFailed:
			parts = append(parts, "failed on this instance")
		case db.AttemptOutcomeCancelled:
			parts = append(parts, "cancelled on this instance")
		}
	}
	if len(parts) == 0 {
		switch job.Status {
		case db.StatusFailed:
			parts = append(parts, "failed")
		case db.StatusDead:
			parts = append(parts, "failed to start")
		case db.StatusCompleted:
			if job.ExitCode != nil && *job.ExitCode != 0 {
				parts = append(parts, fmt.Sprintf("exit %d", *job.ExitCode))
			}
		}
	}
	return strings.Join(parts, "; ")
}

func diagnosisMessageForJob(job *db.Job) string {
	return formatDiagnosisSummary(ResolveJobDiagnosis(job))
}

func humanizeFailureReason(reason string) string {
	// failure_reason is conceptually a one-line classification (see the
	// FailureReason enum in specs/job-lifecycle.allium). Defend against
	// older or buggy writers that may have stuffed multi-line content
	// here — the callers print this on single-line `Reason:` / TUI
	// footer rows, and embedded newlines corrupt the surrounding layout.
	reason = firstLine(reason)
	switch reason {
	case "":
		return ""
	case "gpu_oom":
		return "GPU out of memory"
	case "oom":
		return "host out of memory"
	case "disk_full":
		return "disk full"
	case "timeout":
		return "timed out"
	case "setup_timeout":
		return "setup phase exceeded its time budget (slow rental network/download or heavy uv sync); will retry on a different rental"
	case db.FailureReasonInfraTorchPreflightFailed:
		return "CUDA probe failed before user code started"
	case db.FailureReasonTorchPreflightEnvironmentFailed:
		return "torch preflight could not start its Python environment"
	case db.FailureReasonTorchPreflightImportFailed:
		return "torch preflight could not import torch"
	case "run_timeout":
		return "run phase exceeded its --max-time budget"
	case "killed_stdout_silence":
		return "killed: no stdout output for the silence-watchdog timeout"
	case "killed_gpu_idle":
		return "killed: GPU idle for the GPU-watchdog timeout"
	default:
		return strings.ReplaceAll(reason, "_", " ")
	}
}

func formatCampaignDiagnosisReport(report *campaignDiagnosisReport) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Campaign %d — %s\n", report.Campaign.ID, report.Campaign.Status)
	fmt.Fprintf(&b, "%d instance(s)\n", len(report.Instances))

	for _, inst := range report.Instances {
		statusLabel := campaign.DisplayInstanceStatusWithReason(inst.Instance)

		fmt.Fprintf(&b, "\nInstance %s — %s — %s\n", ids.FormatInstanceID(inst.Instance.ID), displayInstanceGPU(inst.Instance), statusLabel)
		fmt.Fprintf(&b, "Cause: %s\n", inst.Summary)

		if len(inst.JobFindings) == 0 {
			continue
		}
		for _, finding := range inst.JobFindings {
			fmt.Fprintf(&b, "Job #%d: %s\n", finding.JobID, finding.Summary)
		}
	}

	return b.String()
}

func displayInstanceGPU(inst *db.Launch) string {
	if inst == nil {
		return "unknown GPU"
	}
	if spec := inst.DisplayGPUBrief(); spec != "" {
		return spec
	}
	return "unknown GPU"
}

func appendUnique(parts []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return parts
	}
	for _, existing := range parts {
		if strings.EqualFold(existing, value) {
			return parts
		}
	}
	return append(parts, value)
}

func firstLine(s string) string {
	line := strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if len(line) > 120 {
		return line[:117] + "..."
	}
	return line
}

func countOutcome(outcomes map[int64]string, want string) int {
	count := 0
	for _, outcome := range outcomes {
		if outcome == want {
			count++
		}
	}
	return count
}
