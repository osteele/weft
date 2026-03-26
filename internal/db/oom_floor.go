package db

import (
	"database/sql"
)

// OOMFloor returns the minimum GPU memory (in GB) needed to avoid repeating
// a prior GPU OOM failure for a given command. Returns 0 if no OOM history
// exists for this command.
//
// The floor is max(gpu_capacity_gb across all OOM failures) + 1, meaning
// the job needs a GPU strictly larger than the one it OOM'd on.
func OOMFloor(db *sql.DB, command string) (int, error) {
	var floor sql.NullInt64
	err := db.QueryRow(`
		SELECT MAX(CAST(json_extract(ja.error_diagnosis, '$.gpu_capacity_gb') AS INTEGER)) + 1
		FROM job_attempts ja
		JOIN jobs j ON ja.job_id = j.id
		WHERE j.command = ?
		  AND json_extract(ja.error_diagnosis, '$.pattern') = 'gpu_oom'
		  AND json_extract(ja.error_diagnosis, '$.gpu_capacity_gb') > 0
	`, command).Scan(&floor)
	if err != nil {
		return 0, err
	}
	if !floor.Valid {
		return 0, nil
	}
	return int(floor.Int64), nil
}
