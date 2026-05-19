// Package remediation provides error pattern matching and auto-remediation
// for failed jobs. It inspects job logs to diagnose failures and, when possible,
// automatically fixes the issue (e.g., pre-staging missing data) and retries.
package remediation

import (
	"encoding/json"
	"time"
)

// GPUOOMProcess holds per-process VRAM usage parsed from CUDA OOM logs.
type GPUOOMProcess struct {
	PID       int     `json:"pid"`
	MemoryGiB float64 `json:"memory_gib"`
}

// ErrorDiagnosis describes a diagnosed error from a failed job's log output.
type ErrorDiagnosis struct {
	Pattern           string         `json:"pattern"`                // e.g., "gpu_oom", "module_not_found"
	Category          string         `json:"category"`               // "data", "code", "environment"
	Message           string         `json:"message"`                // human-readable summary
	Solution          string         `json:"solution,omitempty"`     // next action to make the job runnable
	MissingAssets     []string       `json:"missing_assets"`         // for data errors: ["hf:meta-llama/Llama-3-8B"]
	Remediable        bool           `json:"remediable"`             // can the coordinator auto-fix this?
	Details           string         `json:"-"`                      // raw error text that matched
	StructuredDetails map[string]any `json:"-"`                      // per-pattern structured detail object
	DetectedBy        string         `json:"detected_by,omitempty"`  // "agent", "wrapper", "post", "backfill"
	DetectedAt        int64          `json:"detected_at,omitempty"`  // unix epoch
	Confidence        float64        `json:"confidence,omitempty"`   // heuristic confidence, 0..1
	MatchedText       string         `json:"matched_text,omitempty"` // exact matching log fragment
	EvidenceTailChars int            `json:"evidence_tail_chars,omitempty"`
	GPUCapacityGB     int            `json:"gpu_capacity_gb,omitempty"` // for gpu_oom: total GPU memory (GB) of the device that OOM'd
	// GPU OOM attribution parsed from framework error output.
	GPUOOMProcesses   []GPUOOMProcess `json:"gpu_oom_processes,omitempty"`
	GPUOOMMainPID     int             `json:"gpu_oom_main_pid,omitempty"`
	GPUOOMExtraPID    int             `json:"gpu_oom_extra_pid,omitempty"`
	GPUOOMExtraGiB    float64         `json:"gpu_oom_extra_memory_gib,omitempty"`
	GPUOOMHintDeltaGB int             `json:"gpu_oom_hint_mem_delta_gb,omitempty"`
	GPUOOMNotes       string          `json:"gpu_oom_notes,omitempty"`
}

type errorDiagnosisJSON struct {
	Pattern           string          `json:"pattern"`
	Category          string          `json:"category,omitempty"`
	Message           string          `json:"message,omitempty"`
	Solution          string          `json:"solution,omitempty"`
	MissingAssets     []string        `json:"missing_assets,omitempty"`
	Remediable        bool            `json:"remediable,omitempty"`
	Details           json.RawMessage `json:"details,omitempty"`
	DetectedBy        string          `json:"detected_by,omitempty"`
	DetectedAt        int64           `json:"detected_at,omitempty"`
	Confidence        float64         `json:"confidence,omitempty"`
	MatchedText       string          `json:"matched_text,omitempty"`
	EvidenceTailChars int             `json:"evidence_tail_chars,omitempty"`
	GPUCapacityGB     int             `json:"gpu_capacity_gb,omitempty"`
	GPUOOMProcesses   []GPUOOMProcess `json:"gpu_oom_processes,omitempty"`
	GPUOOMMainPID     int             `json:"gpu_oom_main_pid,omitempty"`
	GPUOOMExtraPID    int             `json:"gpu_oom_extra_pid,omitempty"`
	GPUOOMExtraGiB    float64         `json:"gpu_oom_extra_memory_gib,omitempty"`
	GPUOOMHintDeltaGB int             `json:"gpu_oom_hint_mem_delta_gb,omitempty"`
	GPUOOMNotes       string          `json:"gpu_oom_notes,omitempty"`
}

// MarshalJSON writes the extended schema, preserving gpu_capacity_gb at the
// top level for the OOM-floor query while using details as a structured object.
func (d ErrorDiagnosis) MarshalJSON() ([]byte, error) {
	if d.MatchedText == "" {
		d.MatchedText = d.Details
	}
	out := errorDiagnosisJSON{
		Pattern:           d.Pattern,
		Category:          d.Category,
		Message:           d.Message,
		Solution:          d.Solution,
		MissingAssets:     d.MissingAssets,
		Remediable:        d.Remediable,
		DetectedBy:        d.DetectedBy,
		DetectedAt:        d.DetectedAt,
		Confidence:        d.Confidence,
		MatchedText:       d.MatchedText,
		EvidenceTailChars: d.EvidenceTailChars,
		GPUCapacityGB:     d.GPUCapacityGB,
		GPUOOMProcesses:   d.GPUOOMProcesses,
		GPUOOMMainPID:     d.GPUOOMMainPID,
		GPUOOMExtraPID:    d.GPUOOMExtraPID,
		GPUOOMExtraGiB:    d.GPUOOMExtraGiB,
		GPUOOMHintDeltaGB: d.GPUOOMHintDeltaGB,
		GPUOOMNotes:       d.GPUOOMNotes,
	}
	if len(d.StructuredDetails) > 0 {
		data, err := json.Marshal(d.StructuredDetails)
		if err != nil {
			return nil, err
		}
		out.Details = data
	}
	return json.Marshal(out)
}

// UnmarshalJSON accepts both the extended details object and older records
// where details was a raw matched-text string.
func (d *ErrorDiagnosis) UnmarshalJSON(data []byte) error {
	var raw errorDiagnosisJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*d = ErrorDiagnosis{
		Pattern:           raw.Pattern,
		Category:          raw.Category,
		Message:           raw.Message,
		Solution:          raw.Solution,
		MissingAssets:     raw.MissingAssets,
		Remediable:        raw.Remediable,
		DetectedBy:        raw.DetectedBy,
		DetectedAt:        raw.DetectedAt,
		Confidence:        raw.Confidence,
		MatchedText:       raw.MatchedText,
		EvidenceTailChars: raw.EvidenceTailChars,
		GPUCapacityGB:     raw.GPUCapacityGB,
		GPUOOMProcesses:   raw.GPUOOMProcesses,
		GPUOOMMainPID:     raw.GPUOOMMainPID,
		GPUOOMExtraPID:    raw.GPUOOMExtraPID,
		GPUOOMExtraGiB:    raw.GPUOOMExtraGiB,
		GPUOOMHintDeltaGB: raw.GPUOOMHintDeltaGB,
		GPUOOMNotes:       raw.GPUOOMNotes,
	}
	if len(raw.Details) > 0 {
		var text string
		if err := json.Unmarshal(raw.Details, &text); err == nil {
			d.Details = text
		} else {
			var details map[string]any
			if err := json.Unmarshal(raw.Details, &details); err == nil {
				d.StructuredDetails = details
			}
		}
	}
	if d.Details == "" {
		d.Details = d.MatchedText
	}
	return nil
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
	if d := matchFailurePattern(logContent, "post"); d != nil && d.Pattern != "unknown" {
		return d
	}
	return nil
}

// DiagnoseFailedAttemptFromLog always returns a diagnosis for a failed attempt,
// using pattern "unknown" when no specific heuristic matches.
func DiagnoseFailedAttemptFromLog(logContent, detectedBy string) *ErrorDiagnosis {
	for _, p := range dataPatterns {
		if d := p.Match(logContent); d != nil {
			stampDiagnosis(d, logContent, detectedBy, 0.85)
			return d
		}
	}
	return matchFailurePattern(logContent, detectedBy)
}

func stampDiagnosis(d *ErrorDiagnosis, logContent, detectedBy string, confidence float64) {
	if d == nil {
		return
	}
	if d.DetectedBy == "" {
		d.DetectedBy = detectedBy
	}
	if d.DetectedAt == 0 {
		d.DetectedAt = time.Now().Unix()
	}
	if d.Confidence == 0 {
		d.Confidence = confidence
	}
	if d.MatchedText == "" {
		d.MatchedText = d.Details
	}
	d.EvidenceTailChars = len(evidenceTail(logContent))
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
