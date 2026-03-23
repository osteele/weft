// Package remediation provides error pattern matching and auto-remediation
// for failed jobs. It inspects job logs to diagnose failures and, when possible,
// automatically fixes the issue (e.g., pre-staging missing data) and retries.
package remediation

import "encoding/json"

// ErrorDiagnosis describes a diagnosed error from a failed job's log output.
type ErrorDiagnosis struct {
	Pattern       string   `json:"pattern"`                   // e.g., "missing_hf_model", "missing_import"
	Category      string   `json:"category"`                  // "data", "code", "environment"
	Message       string   `json:"message"`                   // human-readable summary
	MissingAssets []string `json:"missing_assets"`            // for data errors: ["hf:meta-llama/Llama-3-8B"]
	Remediable    bool     `json:"remediable"`                // can the coordinator auto-fix this?
	Details       string   `json:"details"`                   // raw error text that matched
	GPUCapacityGB int      `json:"gpu_capacity_gb,omitempty"` // for gpu_oom: total GPU memory (GB) of the device that OOM'd
}

// DiagnoseFromLog scans log content for known error patterns and returns
// a diagnosis if one matches. Returns nil if no known pattern matches.
// Data patterns take priority over code patterns.
func DiagnoseFromLog(logContent string) *ErrorDiagnosis {
	// Try data patterns first (higher priority, remediable)
	for _, p := range dataPatterns {
		if d := p.Match(logContent); d != nil {
			return d
		}
	}
	// Then code patterns
	for _, p := range codePatterns {
		if d := p.Match(logContent); d != nil {
			return d
		}
	}
	// Then environment patterns
	for _, p := range envPatterns {
		if d := p.Match(logContent); d != nil {
			return d
		}
	}
	return nil
}

// MarshalDiagnosis serializes an ErrorDiagnosis to JSON for DB storage.
func MarshalDiagnosis(d *ErrorDiagnosis) (string, error) {
	data, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// UnmarshalDiagnosis deserializes an ErrorDiagnosis from JSON.
func UnmarshalDiagnosis(s string) (*ErrorDiagnosis, error) {
	if s == "" {
		return nil, nil
	}
	var d ErrorDiagnosis
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		return nil, err
	}
	return &d, nil
}
