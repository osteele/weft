package ops

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/session"
	"github.com/osteele/weft/internal/ssh"
	"github.com/osteele/weft/internal/telemetryarchive"
)

// syncJobTimeseries fetches the timeseries JSONL from a remote host and inserts
// new samples into the local job_timeseries table.
func syncJobTimeseries(database *sql.DB, job *db.Job, timeout time.Duration) error {
	if job == nil {
		return nil
	}
	if db.IsTerminalStatus(job.Status) && job.LatestRunID != nil {
		obj, objErr := db.GetRawTelemetryObject(database, *job.LatestRunID, db.TimeseriesRawKind)
		summary, summaryErr := db.GetTimeseriesSummaryByRun(database, *job.LatestRunID)
		if objErr != nil {
			return objErr
		}
		if summaryErr != nil {
			return summaryErr
		}
		if obj != nil && summary != nil {
			return nil
		}
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

	samples := db.ParseTimeseriesJSONL(stdout, lastTS, tenant)

	if len(samples) == 0 {
		return nil
	}

	if err := db.InsertTimeseries(database, job.ID, samples); err != nil {
		return err
	}
	if job.LatestRunID != nil {
		if err := db.RefreshTimeseriesSummaryFromRows(database, job.ID, *job.LatestRunID); err != nil {
			return err
		}
		if db.IsTerminalStatus(job.Status) {
			allSamples := db.ParseTimeseriesJSONL(stdout, 0, tenant)
			return telemetryarchive.FinalizeTimeseries(database, job.ID, *job.LatestRunID, []byte(stdout), allSamples, telemetryarchive.RemoteCopy{})
		}
	}
	return nil
}
