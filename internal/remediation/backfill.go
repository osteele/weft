package remediation

import (
	"database/sql"
	"log"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
)

// BackfillOOMCapacity enriches existing gpu_oom diagnoses that are missing
// gpu_capacity_gb by looking up the host's GPU capacity from inventory.
// Returns the number of diagnoses updated.
func BackfillOOMCapacity(database *sql.DB, logger *log.Logger) (int, error) {
	rows, err := database.Query(`
		SELECT jr.id, j.host, jr.error_diagnosis
		FROM job_runs jr
		JOIN jobs j ON jr.job_id = j.id
		WHERE jr.error_diagnosis IS NOT NULL
		  AND json_extract(jr.error_diagnosis, '$.pattern') = 'gpu_oom'
		  AND (json_extract(jr.error_diagnosis, '$.gpu_capacity_gb') IS NULL
		    OR json_extract(jr.error_diagnosis, '$.gpu_capacity_gb') = 0)
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
			logger.Printf("backfill: no GPU capacity for host %q (run %d), skipping", r.host, r.runID)
			continue
		}

		diag, err := UnmarshalDiagnosis(r.diagJSON)
		if err != nil {
			logger.Printf("backfill: unmarshal diagnosis for run %d: %v", r.runID, err)
			continue
		}
		diag.GPUCapacityGB = capGB
		newJSON, err := MarshalDiagnosis(diag)
		if err != nil {
			logger.Printf("backfill: marshal diagnosis for run %d: %v", r.runID, err)
			continue
		}

		if err := db.UpdateRunErrorDiagnosis(database, r.runID, newJSON); err != nil {
			logger.Printf("backfill: update run %d: %v", r.runID, err)
			continue
		}
		updated++
	}

	return updated, nil
}
