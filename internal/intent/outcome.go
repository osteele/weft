package intent

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/ssh"
)

// Outcome represents the coordinator's placement decision for an intent.
type Outcome struct {
	IntentID string    `json:"intent_id"`
	JobID    int64     `json:"job_id"`
	Host     string    `json:"host"`
	Reasons  []string  `json:"reasons,omitempty"`
	Error    string    `json:"error,omitempty"`
	Time     time.Time `json:"time"`
}

// OutcomeFilename returns the conventional filename for an outcome.
func OutcomeFilename(intentID string) string {
	return intentID + ".outcome.json"
}

// WriteOutcome writes a placement outcome file to the coordinator's archive directory.
func WriteOutcome(archiveDir string, outcome *Outcome) error {
	data, err := json.Marshal(outcome)
	if err != nil {
		return fmt.Errorf("marshal outcome: %w", err)
	}
	path := archiveDir + "/" + OutcomeFilename(outcome.IntentID)
	return os.WriteFile(path, data, 0644)
}

// ReadOutcome reads a placement outcome from the coordinator host via SSH.
func ReadOutcome(coordinatorHost, archiveDir, intentID string) (*Outcome, error) {
	path := archiveDir + "/" + OutcomeFilename(intentID)
	cmd := fmt.Sprintf("cat %s 2>/dev/null", path)
	stdout, stderr, err := ssh.RunWithTimeout(coordinatorHost, cmd, 5*time.Second)
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			return nil, fmt.Errorf("coordinator unreachable: %w", err)
		}
		return nil, fmt.Errorf("read outcome: %w", err)
	}
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil, nil // no outcome yet
	}
	var outcome Outcome
	if err := json.Unmarshal([]byte(stdout), &outcome); err != nil {
		return nil, fmt.Errorf("parse outcome: %w", err)
	}
	return &outcome, nil
}
