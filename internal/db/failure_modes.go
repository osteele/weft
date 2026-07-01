package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
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

	// FailureReasonInfraPrewarmDownloadFailed marks a weft-owned input staging
	// failure that happened before the user command started. Launch
	// normalization treats this as retryable when the launch itself ends with
	// an infrastructure-side termination reason.
	FailureReasonInfraPrewarmDownloadFailed = "infra_prewarm_download_failed"
)

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
