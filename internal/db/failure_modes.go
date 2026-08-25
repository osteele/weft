package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// FailureModePrediction is a posterior failure probability for one placement
// scope. Probability is a Beta-Binomial posterior mean.
type FailureModePrediction struct {
	Mode        string
	Probability float64
	Failures    int
	Total       int
	Scope       string
}

const (
	failureModePriorAlpha = 1.0
	failureModePriorBeta  = 19.0

	// AutoRequeueMaxInfraFailureAttempts caps automatic retries for terminal
	// infrastructure-classified job failures. Runtime CUDA faults may reflect
	// user kernels that repeatedly trip bad hardware paths, so repeated
	// failures stay failed for diagnosis instead of looping.
	AutoRequeueMaxInfraFailureAttempts = 3

	// Runner-detected forensic reasons live here so lower-level consumers can
	// match them without importing runner and creating a package cycle.
	FailureReasonDiskFull            = "disk_full"
	FailureReasonOOM                 = "oom"
	FailureReasonGPUOOM              = "gpu_oom"
	FailureReasonSegfault            = "segfault"
	FailureReasonAborted             = "aborted"
	FailureReasonKilledSIGKILL       = "killed_sigkill"
	FailureReasonKilledSIGTERM       = "killed_sigterm"
	FailureReasonGPUIdle             = "killed_gpu_idle"
	FailureReasonStdoutSilence       = "killed_stdout_silence"
	FailureReasonSetupTimeout        = "setup_timeout"
	FailureReasonRunTimeout          = "run_timeout"
	FailureReasonCUDADriverTooOld    = "cuda_driver_too_old"
	FailureReasonError               = "error"
	FailureReasonPrewarmFailed       = "prewarm_failed"
	FailureReasonArtifactStageFailed = "artifact_stage_failed"
	FailureReasonR2ResultsNotSynced  = "infra_r2_results_not_synced"
	FailureReasonSourceRestoreFailed = "source_restore_failed"

	FailureReasonGPUCountPreflightFailed     = "gpu_count_preflight_failed"
	FailureReasonPinnedSourceUnavailable     = "pinned_source_unavailable"
	FailureReasonPinnedSourceFetchFailed     = "pinned_source_fetch_failed"
	FailureReasonR2IsolatedSourceUnavailable = "r2_isolated_source_unavailable"
	FailureReasonR2IsolatedSourceFetchFailed = "r2_isolated_source_fetch_failed"
	FailureReasonSourceProvenanceMismatch    = "source_provenance_mismatch"
	FailureReasonCloudAfterFailed            = "cloud_after_failed"

	// FailureReasonInfraPrewarmDownloadFailed marks a weft-owned input staging
	// failure that happened before the user command started. Launch
	// normalization treats this as retryable when the launch itself ends with
	// an infrastructure-side termination reason.
	FailureReasonInfraPrewarmDownloadFailed = "infra_prewarm_download_failed"

	// FailureReasonInfraCloudArtifactStageFailed marks a weft-owned artifact
	// staging failure that happened before the user command started.
	FailureReasonInfraCloudArtifactStageFailed = "infra_cloud_artifact_stage_failed"

	// FailureReasonInfraCUDAHardwareFault marks CUDA failures that indicate a
	// bad rental GPU/interconnect rather than user code.
	FailureReasonInfraCUDAHardwareFault = "infra_cuda_hardware_fault"

	// FailureReasonInfraTorchPreflightFailed marks a weft-owned torch CUDA
	// preflight failure before the user command started.
	FailureReasonInfraTorchPreflightFailed = "infra_torch_preflight_failed"

	// FailureReasonTorchPreflightEnvironmentFailed marks failure to start the
	// Python environment used by the torch preflight. Dependency resolution and
	// interpreter selection happen here, so this is not a CUDA infrastructure
	// diagnosis.
	FailureReasonTorchPreflightEnvironmentFailed = "torch_preflight_environment_failed"

	// FailureReasonTorchPreflightImportFailed marks failure after Python starts
	// but before torch imports. This is a user-environment failure, not evidence
	// that the rental's CUDA hardware is broken.
	FailureReasonTorchPreflightImportFailed = "torch_preflight_import_failed"

	// ExitCodeSetupTimeout is the setup-phase watchdog's exit code, matching
	// timeout(1)'s convention. runner.ExitCodeSetupTimeout aliases this value.
	ExitCodeSetupTimeout = 124
)

var dynamicFailureReasonPattern = regexp.MustCompile(`^(?:exit_-?\d+|signal_[a-z0-9 ]+)$`)

// IsKnownFailureReason reports whether reason satisfies the closed vocabulary
// in specs/job-lifecycle.allium. Empty means that no failure was classified.
func IsKnownFailureReason(reason string) bool {
	if reason == "" {
		return true
	}
	switch reason {
	case FailureReasonDiskFull,
		FailureReasonOOM,
		FailureReasonGPUOOM,
		FailureReasonSegfault,
		FailureReasonAborted,
		FailureReasonKilledSIGKILL,
		FailureReasonKilledSIGTERM,
		FailureReasonGPUIdle,
		FailureReasonStdoutSilence,
		FailureReasonSetupTimeout,
		FailureReasonRunTimeout,
		FailureReasonCUDADriverTooOld,
		FailureReasonError,
		FailureReasonPrewarmFailed,
		FailureReasonArtifactStageFailed,
		FailureReasonR2ResultsNotSynced,
		FailureReasonSourceRestoreFailed,
		FailureReasonGPUCountPreflightFailed,
		FailureReasonPinnedSourceUnavailable,
		FailureReasonPinnedSourceFetchFailed,
		FailureReasonR2IsolatedSourceUnavailable,
		FailureReasonR2IsolatedSourceFetchFailed,
		FailureReasonSourceProvenanceMismatch,
		FailureReasonCloudAfterFailed,
		FailureReasonInfraPrewarmDownloadFailed,
		FailureReasonInfraCloudArtifactStageFailed,
		FailureReasonInfraCUDAHardwareFault,
		FailureReasonInfraTorchPreflightFailed,
		FailureReasonTorchPreflightEnvironmentFailed,
		FailureReasonTorchPreflightImportFailed:
		return true
	}
	return dynamicFailureReasonPattern.MatchString(reason)
}

// SanitizeFailureReason confines the wire and storage value to the vocabulary
// specified in specs/job-lifecycle.allium.
func SanitizeFailureReason(reason string) string {
	if IsKnownFailureReason(reason) {
		return reason
	}
	return FailureReasonError
}

var infraFailureReasons = []string{
	FailureReasonInfraPrewarmDownloadFailed,
	FailureReasonInfraCloudArtifactStageFailed,
	FailureReasonInfraCUDAHardwareFault,
	FailureReasonInfraTorchPreflightFailed,
}

// FailurePhase identifies where in an attempt's lifecycle a failure was
// observed, for infrastructure-vs-user-code classification.
type FailurePhase string

const (
	// PhasePrewarmDownload is weft-owned input staging — the agent's HF
	// prewarm download script — which runs before the user command.
	PhasePrewarmDownload FailurePhase = "prewarm_download"
	// PhaseCloudArtifactStaging is weft-owned R2 artifact staging before the
	// user command starts.
	PhaseCloudArtifactStaging FailurePhase = "cloud_artifact_staging"
	// PhaseGPUCountPreflight is the agent's pre-spend probe that the
	// instance physically exposes the GPUs the job sequence requests.
	PhaseGPUCountPreflight FailurePhase = "gpu_count_preflight"
	// PhaseTorchPreflightEnvironment selects and starts the Python environment.
	PhaseTorchPreflightEnvironment FailurePhase = "torch_preflight_environment"
	// PhaseTorchPreflightImport imports torch inside the selected environment.
	PhaseTorchPreflightImport FailurePhase = "torch_preflight_import"
	// PhaseTorchPreflightCUDA exercises torch's CUDA runtime and assigned GPU.
	PhaseTorchPreflightCUDA FailurePhase = "torch_preflight_cuda"
	// PhaseSetup is a detected setup command (uv sync, etc.) run by the
	// agent's prewarm before the user command.
	PhaseSetup FailurePhase = "setup"
	// PhaseRuntime is the user command itself.
	PhaseRuntime FailurePhase = "runtime"
)

// ClassifyInfraFailure is the single decision point for whether a failure is
// infrastructure-side (retrying on a fresh instance is likely to succeed)
// rather than attributable to user code. It returns the failure_reason to
// record — empty means the caller keeps its own reason — and whether the
// failure classifies as infrastructure. Agent setup-phase decisions
// (cmd/agent/jobloop.go, cmd/agent/gpu_count_probe.go) and the runner's
// runtime classification (internal/runner/job.go) all funnel through here.
// See specs/job-lifecycle.allium § failure classification.
func ClassifyInfraFailure(phase FailurePhase, exitCode int, logTail string) (reason string, infra bool) {
	switch phase {
	case PhasePrewarmDownload:
		// Any failure of weft-owned HF input staging, regardless of exit
		// code: the user command never started, so the fault cannot be
		// user code. The download already survived bounded retries with
		// xet fallback before reaching this classification.
		return FailureReasonInfraPrewarmDownloadFailed, true
	case PhaseCloudArtifactStaging:
		// Artifact staging is weft-owned and happens before the user
		// command starts, so a fetch/stage failure is infrastructure-side.
		return FailureReasonInfraCloudArtifactStageFailed, true
	case PhaseGPUCountPreflight:
		// The provider allocated fewer GPUs than the offer advertised —
		// nothing the user's code did.
		return "", true
	case PhaseTorchPreflightEnvironment:
		return FailureReasonTorchPreflightEnvironmentFailed, false
	case PhaseTorchPreflightImport:
		return FailureReasonTorchPreflightImportFailed, false
	case PhaseTorchPreflightCUDA:
		// Only failures after torch imported are evidence from the CUDA probe.
		return FailureReasonInfraTorchPreflightFailed, true
	case PhaseSetup:
		// Setup timeouts (exit 124) almost always mean rental network
		// throughput, not user error. Other setup failures stay
		// user-attributed.
		return "", exitCode == ExitCodeSetupTimeout
	case PhaseRuntime:
		if CUDAHardwareFaultText(logTail) {
			return FailureReasonInfraCUDAHardwareFault, true
		}
		return "", false
	}
	return "", false
}

// IsInfraFailureReason reports whether a recorded failure_reason marks the
// attempt as an infrastructure-side failure.
func IsInfraFailureReason(reason string) bool {
	for _, infraReason := range infraFailureReasons {
		if reason == infraReason {
			return true
		}
	}
	return false
}

// InfraFailureReasons returns the failure_reason values that mark an attempt
// as infrastructure-side. Callers that need SQL predicates should build them
// from this list so query behavior stays aligned with IsInfraFailureReason.
func InfraFailureReasons() []string {
	return append([]string(nil), infraFailureReasons...)
}

// PredictFailureModes estimates likely failure modes for a command on a host.
// It uses the most specific historical slice with enough observations, then
// shrinks each mode through a Beta prior so small cells do not dominate.
func PredictFailureModes(database *sql.DB, command, host, gpuClass string) ([]FailureModePrediction, error) {
	if database == nil {
		return nil, nil
	}
	scopes := []failureModeScope{
		{name: "command+host", command: command, host: host, minTotal: 3},
		{name: "command", command: command, minTotal: 3},
		{name: "host", host: host, minTotal: 5},
		{name: "gpu_class", gpuClass: gpuClass, minTotal: 5},
		{name: "global", minTotal: 10},
	}
	for _, scope := range scopes {
		if strings.TrimSpace(scope.command) == "" && scope.name != "host" && scope.name != "gpu_class" && scope.name != "global" {
			continue
		}
		if strings.TrimSpace(scope.host) == "" && scope.name == "host" {
			continue
		}
		if strings.TrimSpace(scope.gpuClass) == "" && scope.name == "gpu_class" {
			continue
		}
		preds, total, err := queryFailureModeScope(database, scope)
		if err != nil {
			return nil, err
		}
		if total >= scope.minTotal {
			return preds, nil
		}
	}
	return nil, nil
}

type failureModeScope struct {
	name     string
	command  string
	host     string
	gpuClass string
	minTotal int
}

func queryFailureModeScope(database *sql.DB, scope failureModeScope) ([]FailureModePrediction, int, error) {
	rows, err := database.Query(`
		SELECT
			COALESCE(ja.failure_reason, ''),
			COALESCE(ja.error_diagnosis, ''),
			COALESCE(ja.error_message, ''),
			COALESCE(ja.exit_code, 0)
		FROM job_attempts ja
		JOIN jobs j ON j.id = ja.job_id
		LEFT JOIN launches l ON l.id = ja.launch_id
		WHERE ja.end_time IS NOT NULL
		  AND (? = '' OR j.command = ?)
		  AND (? = '' OR ja.host = ?)
		  AND (? = '' OR lower(COALESCE(l.gpu_class, j.gpu_class, '')) = lower(?))
	`, scope.command, scope.command, scope.host, scope.host, scope.gpuClass, scope.gpuClass)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	total := 0
	failures := make(map[string]int)
	for rows.Next() {
		var failureReason, diagnosis, message string
		var exitCode int
		if err := rows.Scan(&failureReason, &diagnosis, &message, &exitCode); err != nil {
			return nil, 0, err
		}
		total++
		mode := ClassifyFailureMode(failureReason, diagnosis, message, exitCode)
		if mode != "" {
			failures[mode]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	preds := make([]FailureModePrediction, 0, len(failures))
	for mode, count := range failures {
		p := (failureModePriorAlpha + float64(count)) / (failureModePriorAlpha + failureModePriorBeta + float64(total))
		preds = append(preds, FailureModePrediction{
			Mode:        mode,
			Probability: p,
			Failures:    count,
			Total:       total,
			Scope:       scope.name,
		})
	}
	sort.Slice(preds, func(i, j int) bool {
		if preds[i].Probability != preds[j].Probability {
			return preds[i].Probability > preds[j].Probability
		}
		return preds[i].Mode < preds[j].Mode
	})
	return preds, total, nil
}

// ClassifyFailureMode normalizes stored diagnostics into placement-relevant
// failure modes. It is intentionally conservative: success-like attempts return
// an empty mode and do not contribute failures.
func ClassifyFailureMode(failureReason, diagnosis, message string, exitCode int) string {
	pattern := diagnosisPattern(diagnosis)
	if pattern != "" {
		return normalizeFailureMode(pattern)
	}
	text := strings.ToLower(strings.Join([]string{failureReason, diagnosis, message}, "\n"))
	switch {
	case strings.Contains(text, FailureReasonInfraCUDAHardwareFault) || CUDAHardwareFaultText(text):
		return "cuda_hardware_fault"
	case strings.Contains(text, "cuda") && (strings.Contains(text, "out of memory") || strings.Contains(text, "oom")):
		return "gpu_oom"
	case strings.Contains(text, "out of memory") || strings.Contains(text, "oom killer") || failureReason == "oom":
		return "oom"
	case strings.Contains(text, "cuda error") || strings.Contains(text, "illegal memory access") || strings.Contains(text, "cublas") || strings.Contains(text, "cudnn"):
		return "cuda_fault"
	case strings.Contains(text, "module not found") || strings.Contains(text, "no module named") || strings.Contains(text, "modulenotfounderror"):
		return "module_not_found"
	case strings.Contains(text, "ssh") && (strings.Contains(text, "disconnect") || strings.Contains(text, "connection") || strings.Contains(text, "timeout")):
		return "ssh_disconnect"
	case strings.Contains(text, "disk") && (strings.Contains(text, "full") || strings.Contains(text, "no space left")):
		return "disk_full"
	case exitCode == 137:
		return "oom"
	case strings.TrimSpace(failureReason) != "":
		return normalizeFailureMode(failureReason)
	default:
		return ""
	}
}

// CUDAHardwareFaultText reports whether failure text carries the log
// signature of a CUDA hardware fault (bad rental GPU/interconnect): peer
// GPU memory errors, NVLink faults, uncorrectable ECC, or Xid reports.
// Shared by ClassifyInfraFailure (runtime phase) and ClassifyFailureMode.
func CUDAHardwareFaultText(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "peer gpu memory") ||
		strings.Contains(text, "nvlink") ||
		strings.Contains(text, "uncorrectable ecc") ||
		strings.Contains(text, "xid")
}

func diagnosisPattern(diagnosis string) string {
	if strings.TrimSpace(diagnosis) == "" {
		return ""
	}
	var obj struct {
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal([]byte(diagnosis), &obj); err != nil {
		return ""
	}
	return obj.Pattern
}

func normalizeFailureMode(raw string) string {
	mode := strings.ToLower(strings.TrimSpace(raw))
	mode = strings.NewReplacer(" ", "_", "-", "_", ".", "_").Replace(mode)
	switch mode {
	case "", "completed", "success", "ok":
		return ""
	case "gpu_out_of_memory", "cuda_oom":
		return "gpu_oom"
	case "module_not_found_error", "no_module_named":
		return "module_not_found"
	case "ssh_timeout", "connection_lost", "connection_reset":
		return "ssh_disconnect"
	default:
		if len(mode) > 64 {
			return fmt.Sprintf("%.64s", mode)
		}
		return mode
	}
}
