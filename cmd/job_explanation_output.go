package cmd

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/explain"
	"github.com/osteele/weft/internal/ids"
)

func explanationHasHighConfidenceBlocker(x explain.Explanation) bool {
	if !strings.EqualFold(strings.TrimSpace(x.Confidence), "high") {
		return false
	}
	if strings.TrimSpace(x.PrimaryReason) == "" {
		return false
	}
	switch strings.TrimSpace(x.State) {
	case "blocked", "paused", "waiting":
		return true
	default:
		return false
	}
}

func explanationBlockerSummary(x explain.Explanation) string {
	reason := strings.TrimSpace(x.PrimaryReason)
	state := strings.TrimSpace(x.State)
	if state == "" {
		return reason
	}
	return state + ": " + reason
}

func explanationBlockerLabel(x explain.Explanation) string {
	if strings.TrimSpace(x.State) == "waiting" {
		return "Waiting on"
	}
	return "Blocker"
}

func printDiagnoseJobHint(label string, labelWidth int, jobID int64) {
	fmt.Printf("%-*s weft diagnose job %s  # Explain blocker and evidence\n", labelWidth, label+":", ids.FormatJobID(jobID))
}
