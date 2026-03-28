package remediation

import (
	"database/sql"
	"log/slog"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
)

// BackfillOOMCapacity enriches existing gpu_oom diagnoses that are missing
// gpu_capacity_gb by looking up the host's GPU capacity from inventory.
// Returns the number of diagnoses updated.
func BackfillOOMCapacity(database *sql.DB, logger *slog.Logger) (int, error) {
	rows, err := database.Query(`
		SELECT ja.id, ja.host, ja.error_diagnosis
		FROM job_attempts ja
		WHERE ja.error_diagnosis IS NOT NULL
		  AND json_extract(ja.error_diagnosis, '$.pattern') = 'gpu_oom'
		  AND (json_extract(ja.error_diagnosis, '$.gpu_capacity_gb') IS NULL
		    OR json_extract(ja.error_diagnosis, '$.gpu_capacity_gb') = 0)
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type record struct {
		runID    int64
		host     string
		diagJSON string
	}
	var records []record
	for rows.Next() {
		var r record
		if err := rows.Scan(&r.runID, &r.host, &r.diagJSON); err != nil {
			return 0, err
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	updated := 0
	for _, r := range records {
		capGB := inventory.HostMaxGPUMemoryGB(r.host)
		if capGB == 0 {
			logger.Debug("no GPU capacity, skipping", "component", "backfill", "host", r.host, "run_id", r.runID)
			continue
		}

		diag, err := UnmarshalDiagnosis(r.diagJSON)
		if err != nil {
			logger.Debug("failed to unmarshal diagnosis", "component", "backfill", "run_id", r.runID, "error", err)
			continue
		}
		diag.GPUCapacityGB = capGB
		newJSON, err := MarshalDiagnosis(diag)
		if err != nil {
			logger.Debug("failed to marshal diagnosis", "component", "backfill", "run_id", r.runID, "error", err)
			continue
		}

		if err := db.UpdateRunErrorDiagnosis(database, r.runID, newJSON); err != nil {
			logger.Debug("failed to update run diagnosis", "component", "backfill", "run_id", r.runID, "error", err)
			continue
		}
		updated++
	}

	return updated, nil
}
