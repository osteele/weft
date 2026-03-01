package ops

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/session"
	"github.com/osteele/weft/internal/ssh"
)

// syncJobTimeseries fetches the timeseries JSONL from a remote host and inserts
// new samples into the local job_timeseries table.
func syncJobTimeseries(database *sql.DB, job *db.Job, timeout time.Duration) error {
	if job == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	// Determine tenant from backend
	tenant := "multi"
	if job.Backend == db.BackendVastai {
		tenant = "single"
	}

	// Get last synced timestamp to avoid re-fetching
	lastTS, err := db.GetTimeseriesLastTS(database, job.ID)
	if err != nil {
		return fmt.Errorf("get last ts: %w", err)
	}

	// Fetch the timeseries file from remote
	tsFile := session.SimpleTimeseriesFile(job.ID)
	cmd := fmt.Sprintf("cat %s 2>/dev/null", tsFile)
	stdout, _, err := ssh.RunWithTimeout(job.Host, cmd, timeout)
	if err != nil || strings.TrimSpace(stdout) == "" {
		return nil // No timeseries file yet — not an error
	}

	// Parse JSONL and filter to new samples
	var samples []db.TimeseriesSample
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s db.TimeseriesSample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			continue // skip malformed lines
		}
		if s.Ts <= lastTS {
			continue // already synced
		}
		// Override tenant from backend (runner always writes "multi")
		s.Tenant = tenant
		samples = append(samples, s)
	}

	if len(samples) == 0 {
		return nil
	}

	return db.InsertTimeseries(database, job.ID, samples)
}
