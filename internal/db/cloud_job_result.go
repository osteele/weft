package db

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// KillReasonUserKill is the kill_reason recorded in a job's completion record
// when weft itself stopped the job in response to a user kill or cancel
// (runner.KillReasonUserKill aliases this value; runner imports db, not the
// reverse). A completion carrying this reason confirms the user's stop rather
// than reporting a self-failure, so completion ingestion preserves the user's
// killed/canceled status instead of recording failed.
const KillReasonUserKill = "user_kill"

// ParseCloudJobResult reads the exit code, start time, end time, failure
// reason, and kill reason from a downloaded R2 results directory.
// Returns nil exitCode if no valid result was found.
func ParseCloudJobResult(tmpDir, jobIDStr string) (exitCode *int, startTimeUnix, endTimeUnix int64, failureReason, killReason string) {
	// Try completion JSON (agent format: <jobID>.completion.json)
	completionPath := filepath.Join(tmpDir, jobIDStr+".completion.json")
	if data, err := os.ReadFile(completionPath); err == nil {
		var rec struct {
			ExitCode      int    `json:"exit_code"`
			StartTime     int64  `json:"start_time"`
			EndTime       int64  `json:"end_time"`
			FailureReason string `json:"failure_reason"`
			KillReason    string `json:"kill_reason"`
		}
		if json.Unmarshal(data, &rec) == nil {
			return &rec.ExitCode, rec.StartTime, rec.EndTime, rec.FailureReason, rec.KillReason
		}
	}

	// Fallback: <jobID>.status (exit code as text)
	statusPath := filepath.Join(tmpDir, jobIDStr+".status")
	if data, err := os.ReadFile(statusPath); err == nil {
		var code int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &code); err == nil {
			return &code, 0, 0, "", ""
		}
	}

	// Legacy fallback: standalone exit_code file
	if data, err := os.ReadFile(filepath.Join(tmpDir, "exit_code")); err == nil {
		code, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil {
			endTimeBytes, _ := os.ReadFile(filepath.Join(tmpDir, "end_time"))
			et, _ := strconv.ParseInt(strings.TrimSpace(string(endTimeBytes)), 10, 64)
			return &code, 0, et, "", ""
		}
	}

	return nil, 0, 0, "", ""
}
