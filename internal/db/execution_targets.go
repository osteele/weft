package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type ExecutionTargetKind string

const (
	ExecutionTargetInventoryHost  ExecutionTargetKind = "inventory_host"
	ExecutionTargetRentalInstance ExecutionTargetKind = "rental_instance"
)

type ExecutionTargetStatus string

const (
	ExecutionTargetProvisioning ExecutionTargetStatus = "provisioning"
	ExecutionTargetReady        ExecutionTargetStatus = "ready"
	ExecutionTargetRunning      ExecutionTargetStatus = "running"
	ExecutionTargetDraining     ExecutionTargetStatus = "draining"
	ExecutionTargetTerminated   ExecutionTargetStatus = "terminated"
)

type ExecutionTarget struct {
	ID             int64
	Kind           ExecutionTargetKind
	Host           string
	LaunchID       *int64
	Status         ExecutionTargetStatus
	GPUClass       string
	GPUMemGB       int
	NumGPUs        int
	Cordoned       bool
	CordonReason   string
	CordonedAt     *int64
	DeadlineUnix   *int64
	CurrentJobID   *int64
	QueueDepth     int
	CreatedAt      int64
	UpdatedAt      int64
	LastObservedAt *int64
}

const executionTargetSelectColumns = `id, kind, host, launch_id, status, gpu_class, gpu_mem_gb, num_gpus,
	cordoned, cordon_reason, cordoned_at, deadline_unix, current_job_id, queue_depth,
	created_at, updated_at, last_observed_at`

func executionTargetStatusValues() []string {
	return []string{
		string(ExecutionTargetProvisioning),
		string(ExecutionTargetReady),
		string(ExecutionTargetRunning),
		string(ExecutionTargetDraining),
		string(ExecutionTargetTerminated),
	}
}

func executionTargetKindValues() []string {
	return []string{
		string(ExecutionTargetInventoryHost),
		string(ExecutionTargetRentalInstance),
	}
}

func createExecutionTargetsTableSQL(ifNotExists bool) string {
	ifClause := ""
	if ifNotExists {
		ifClause = "IF NOT EXISTS "
	}
	return fmt.Sprintf(`CREATE TABLE %sexecution_targets (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,
		host TEXT NOT NULL DEFAULT '',
		launch_id INTEGER REFERENCES launches(id),
		status TEXT NOT NULL DEFAULT 'ready',
		gpu_class TEXT,
		gpu_mem_gb INTEGER NOT NULL DEFAULT 0,
		num_gpus INTEGER NOT NULL DEFAULT 0,
		cordoned INTEGER NOT NULL DEFAULT 0,
		cordon_reason TEXT,
		cordoned_at INTEGER,
		deadline_unix INTEGER,
		current_job_id INTEGER REFERENCES jobs(id),
		queue_depth INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		last_observed_at INTEGER,
		CONSTRAINT execution_targets_kind_check CHECK (%s),
		CONSTRAINT execution_targets_status_check CHECK (%s),
		CONSTRAINT execution_targets_shape_check CHECK (
			(kind = 'inventory_host' AND host != '' AND launch_id IS NULL)
			OR (kind = 'rental_instance' AND launch_id IS NOT NULL)
		)
	)`, ifClause,
		statusCheckConstraintSQL("kind", executionTargetKindValues(), false),
		statusCheckConstraintSQL("status", executionTargetStatusValues(), false))
}

func initExecutionTargetsSchema(database *sql.DB) error {
	if _, err := database.Exec(createExecutionTargetsTableSQL(true)); err != nil {
		return err
	}
	for _, stmt := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_execution_targets_inventory_host ON execution_targets(host) WHERE kind = 'inventory_host'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_execution_targets_launch ON execution_targets(launch_id) WHERE kind = 'rental_instance'`,
		`CREATE INDEX IF NOT EXISTS idx_execution_targets_kind_status ON execution_targets(kind, status)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_targets_cordoned ON execution_targets(cordoned)`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			return err
		}
	}
	return SyncExecutionTargets(database)
}

func ensureExecutionTargetForAttempt(execer dbExecer, host string, launchID *int64) (any, error) {
	if launchID != nil && *launchID > 0 {
		if _, err := execer.Exec(`
			INSERT INTO execution_targets
				(kind, host, launch_id, status, gpu_class, gpu_mem_gb, num_gpus, cordoned, cordon_reason, cordoned_at,
				 deadline_unix, created_at, updated_at, last_observed_at)
			SELECT 'rental_instance',
			       'rental:' || l.id,
			       l.id,
			       CASE
			         WHEN l.status IN ('planned','launching') THEN 'provisioning'
			         WHEN l.status IN ('running','grace') THEN 'running'
			         WHEN l.status = 'paused' THEN 'draining'
			         ELSE 'terminated'
			       END,
			       l.gpu_class,
			       COALESCE(l.gpu_mem_gb, 0),
			       COALESCE(l.num_gpus, 0),
			       COALESCE(l.cordoned, 0),
			       l.cordon_reason,
			       l.cordoned_at,
			       l.grace_deadline,
			       l.created_at,
			       strftime('%s','now'),
			       COALESCE(l.agent_ready_at_unix, l.ready_at, l.launched_at, l.created_at)
			  FROM launches l
			 WHERE l.id = ?
			ON CONFLICT(launch_id) WHERE kind = 'rental_instance' DO UPDATE SET
				status = excluded.status,
				gpu_class = excluded.gpu_class,
				gpu_mem_gb = excluded.gpu_mem_gb,
				num_gpus = excluded.num_gpus,
				cordoned = excluded.cordoned,
				cordon_reason = excluded.cordon_reason,
				cordoned_at = excluded.cordoned_at,
				deadline_unix = excluded.deadline_unix,
				updated_at = excluded.updated_at,
				last_observed_at = excluded.last_observed_at`, *launchID); err != nil {
			return nil, err
		}
		var targetID int64
		if err := execer.QueryRow(
			`SELECT id FROM execution_targets WHERE kind = 'rental_instance' AND launch_id = ?`,
			*launchID,
		).Scan(&targetID); err != nil {
			return nil, err
		}
		return targetID, nil
	}
	host = strings.TrimSpace(host)
	if host == "" || IsLaunchHost(host) {
		return nil, nil
	}
	now := time.Now().Unix()
	if _, err := execer.Exec(`
		INSERT INTO execution_targets (kind, host, status, created_at, updated_at, last_observed_at)
		VALUES ('inventory_host', ?, 'ready', ?, ?, ?)
		ON CONFLICT(host) WHERE kind = 'inventory_host' DO UPDATE SET
			updated_at = excluded.updated_at,
			last_observed_at = excluded.last_observed_at`,
		host, now, now, now); err != nil {
		return nil, err
	}
	var targetID int64
	if err := execer.QueryRow(
		`SELECT id FROM execution_targets WHERE kind = 'inventory_host' AND host = ?`,
		host,
	).Scan(&targetID); err != nil {
		return nil, err
	}
	return targetID, nil
}

func SyncExecutionTargets(database *sql.DB) error {
	if database == nil {
		return nil
	}
	now := time.Now().Unix()
	if _, err := database.Exec(`
		INSERT INTO execution_targets (kind, host, status, created_at, updated_at, last_observed_at)
		SELECT 'inventory_host', hic.name, 'ready', ?, ?, hic.last_updated
		  FROM host_info_cache hic
		 WHERE hic.name != ''
		   AND hic.name NOT LIKE 'vastai:%'
		   AND hic.name NOT LIKE 'runpod:%'
		ON CONFLICT(host) WHERE kind = 'inventory_host' DO UPDATE SET
			updated_at = excluded.updated_at,
			last_observed_at = excluded.last_observed_at
	`, now, now); err != nil {
		return err
	}
	if _, err := database.Exec(`
		INSERT INTO execution_targets
			(kind, host, launch_id, status, gpu_class, gpu_mem_gb, num_gpus, cordoned, cordon_reason, cordoned_at,
			 deadline_unix, created_at, updated_at, last_observed_at)
		SELECT 'rental_instance',
		       'rental:' || l.id,
		       l.id,
		       CASE
		         WHEN l.status IN ('planned','launching') THEN 'provisioning'
		         WHEN l.status IN ('running','grace') THEN 'running'
		         WHEN l.status = 'paused' THEN 'draining'
		         ELSE 'terminated'
		       END,
		       l.gpu_class,
		       COALESCE(l.gpu_mem_gb, 0),
		       COALESCE(l.num_gpus, 0),
		       COALESCE(l.cordoned, 0),
		       l.cordon_reason,
		       l.cordoned_at,
		       l.grace_deadline,
		       l.created_at,
		       ?,
		       COALESCE(l.agent_ready_at_unix, l.ready_at, l.launched_at, l.created_at)
		  FROM launches l
		 WHERE true
		ON CONFLICT(launch_id) WHERE kind = 'rental_instance' DO UPDATE SET
			status = excluded.status,
			gpu_class = excluded.gpu_class,
			gpu_mem_gb = excluded.gpu_mem_gb,
			num_gpus = excluded.num_gpus,
			cordoned = excluded.cordoned,
			cordon_reason = excluded.cordon_reason,
			cordoned_at = excluded.cordoned_at,
			deadline_unix = excluded.deadline_unix,
			updated_at = excluded.updated_at,
			last_observed_at = excluded.last_observed_at
	`, now); err != nil {
		return err
	}
	if _, err := database.Exec(`
		UPDATE launches
		   SET target_id = (
		       SELECT et.id
		         FROM execution_targets et
		        WHERE et.kind = 'rental_instance'
		          AND et.launch_id = launches.id
		   )
		 WHERE target_id IS NULL
		   AND EXISTS (
		       SELECT 1
		         FROM execution_targets et
		        WHERE et.kind = 'rental_instance'
		          AND et.launch_id = launches.id
		   )`); err != nil {
		return err
	}
	return refreshExecutionTargetOccupancy(database)
}

func refreshExecutionTargetOccupancy(database *sql.DB) error {
	if ok, err := relationExists(database, "job_status"); err != nil || !ok {
		return err
	}
	_, err := database.Exec(`
		UPDATE execution_targets
		   SET current_job_id = (
		           SELECT MIN(js.id)
		             FROM job_status js
		            WHERE js.effective_target_kind = execution_targets.kind
		              AND ((execution_targets.kind = 'inventory_host' AND js.host = execution_targets.host)
		                   OR (execution_targets.kind = 'rental_instance' AND js.launch_id = execution_targets.launch_id))
		              AND js.status = 'running'
		       ),
		       queue_depth = (
		           SELECT COUNT(*)
		             FROM job_status js
		            WHERE js.effective_target_kind = execution_targets.kind
		              AND ((execution_targets.kind = 'inventory_host' AND js.host = execution_targets.host)
		                   OR (execution_targets.kind = 'rental_instance' AND js.launch_id = execution_targets.launch_id))
		              AND js.status IN ('queued', 'pending_placement')
		       )`)
	return err
}

func relationExists(database *sql.DB, name string) (bool, error) {
	var count int
	err := database.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE name = ? AND type IN ('table','view')`,
		name,
	).Scan(&count)
	return count > 0, err
}

func UpsertInventoryExecutionTarget(database *sql.DB, host string) error {
	host = strings.TrimSpace(host)
	if database == nil || host == "" || IsLaunchHost(host) {
		return nil
	}
	now := time.Now().Unix()
	_, err := database.Exec(`
		INSERT INTO execution_targets (kind, host, status, created_at, updated_at, last_observed_at)
		VALUES ('inventory_host', ?, 'ready', ?, ?, ?)
		ON CONFLICT(host) WHERE kind = 'inventory_host' DO UPDATE SET
			updated_at = excluded.updated_at,
			last_observed_at = excluded.last_observed_at`,
		host, now, now, now)
	return err
}

func EnsureRentalExecutionTarget(database *sql.DB, launchID int64) error {
	if database == nil || launchID <= 0 {
		return nil
	}
	_, err := database.Exec(`
		INSERT INTO execution_targets
			(kind, host, launch_id, status, gpu_class, gpu_mem_gb, num_gpus, cordoned, cordon_reason, cordoned_at,
			 deadline_unix, created_at, updated_at, last_observed_at)
		SELECT 'rental_instance',
		       'rental:' || l.id,
		       l.id,
		       CASE
		         WHEN l.status IN ('planned','launching') THEN 'provisioning'
		         WHEN l.status IN ('running','grace') THEN 'running'
		         WHEN l.status = 'paused' THEN 'draining'
		         ELSE 'terminated'
		       END,
		       l.gpu_class,
		       COALESCE(l.gpu_mem_gb, 0),
		       COALESCE(l.num_gpus, 0),
		       COALESCE(l.cordoned, 0),
		       l.cordon_reason,
		       l.cordoned_at,
		       l.grace_deadline,
		       l.created_at,
		       strftime('%s','now'),
		       COALESCE(l.agent_ready_at_unix, l.ready_at, l.launched_at, l.created_at)
		  FROM launches l
		 WHERE l.id = ?
		ON CONFLICT(launch_id) WHERE kind = 'rental_instance' DO UPDATE SET
			status = excluded.status,
			gpu_class = excluded.gpu_class,
			gpu_mem_gb = excluded.gpu_mem_gb,
			num_gpus = excluded.num_gpus,
			cordoned = excluded.cordoned,
			cordon_reason = excluded.cordon_reason,
			cordoned_at = excluded.cordoned_at,
			deadline_unix = excluded.deadline_unix,
			updated_at = excluded.updated_at,
			last_observed_at = excluded.last_observed_at`, launchID)
	if err != nil {
		return err
	}
	_, err = database.Exec(`
		UPDATE launches
		   SET target_id = (
		       SELECT id FROM execution_targets
		        WHERE kind = 'rental_instance' AND launch_id = ?
		   )
		 WHERE id = ?`, launchID, launchID)
	return err
}

func SetExecutionTargetCordoned(database *sql.DB, targetID int64, cordoned bool, reason string) error {
	var kind string
	var launchID sql.NullInt64
	if err := database.QueryRow(`SELECT kind, launch_id FROM execution_targets WHERE id = ?`, targetID).Scan(&kind, &launchID); err != nil {
		return err
	}
	if kind == string(ExecutionTargetRentalInstance) && launchID.Valid {
		return SetLaunchCordoned(database, launchID.Int64, cordoned, reason)
	}
	return setInventoryExecutionTargetCordoned(database, targetID, cordoned, reason)
}

func SetInventoryExecutionTargetCordoned(database *sql.DB, host string, cordoned bool, reason string) error {
	if err := UpsertInventoryExecutionTarget(database, host); err != nil {
		return err
	}
	var id int64
	if err := database.QueryRow(`SELECT id FROM execution_targets WHERE kind = 'inventory_host' AND host = ?`, host).Scan(&id); err != nil {
		return err
	}
	return setInventoryExecutionTargetCordoned(database, id, cordoned, reason)
}

func setInventoryExecutionTargetCordoned(database *sql.DB, targetID int64, cordoned bool, reason string) error {
	flag := 0
	status := ExecutionTargetReady
	var reasonArg, cordonedAtArg any
	if cordoned {
		flag = 1
		status = ExecutionTargetDraining
		if reason != "" {
			reasonArg = reason
		}
		cordonedAtArg = time.Now().Unix()
	}
	res, err := database.Exec(
		`UPDATE execution_targets SET cordoned = ?, cordon_reason = ?, cordoned_at = ?, status = ?, updated_at = ? WHERE id = ? AND kind = 'inventory_host'`,
		flag, reasonArg, cordonedAtArg, string(status), time.Now().Unix(), targetID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func IsInventoryExecutionTargetCordoned(database *sql.DB, host string) (bool, string, error) {
	host = strings.TrimSpace(host)
	if database == nil || host == "" || IsLaunchHost(host) {
		return false, "", nil
	}
	if err := UpsertInventoryExecutionTarget(database, host); err != nil {
		return false, "", err
	}
	var cordoned int
	var reason sql.NullString
	err := database.QueryRow(
		`SELECT cordoned, cordon_reason FROM execution_targets WHERE kind = 'inventory_host' AND host = ?`,
		host).Scan(&cordoned, &reason)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if reason.Valid {
		return cordoned != 0, reason.String, nil
	}
	return cordoned != 0, "", nil
}

func ListExecutionTargets(database *sql.DB) ([]*ExecutionTarget, error) {
	if err := SyncExecutionTargets(database); err != nil {
		return nil, err
	}
	rows, err := database.Query(`SELECT ` + executionTargetSelectColumns + ` FROM execution_targets ORDER BY kind ASC, updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []*ExecutionTarget
	for rows.Next() {
		t, err := scanExecutionTarget(rows)
		if err != nil {
			return nil, err
		}
		targets = append(targets, t)
	}
	return targets, rows.Err()
}

func GetExecutionTarget(database *sql.DB, id int64) (*ExecutionTarget, error) {
	if err := SyncExecutionTargets(database); err != nil {
		return nil, err
	}
	row := database.QueryRow(`SELECT `+executionTargetSelectColumns+` FROM execution_targets WHERE id = ?`, id)
	t, err := scanExecutionTarget(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}

type executionTargetScanner interface {
	Scan(dest ...any) error
}

func scanExecutionTarget(row executionTargetScanner) (*ExecutionTarget, error) {
	var t ExecutionTarget
	var launchID, cordonedAt, deadline, currentJob, lastObserved sql.NullInt64
	var gpuClass, reason sql.NullString
	var cordoned int
	if err := row.Scan(
		&t.ID, &t.Kind, &t.Host, &launchID, &t.Status, &gpuClass, &t.GPUMemGB, &t.NumGPUs,
		&cordoned, &reason, &cordonedAt, &deadline, &currentJob, &t.QueueDepth,
		&t.CreatedAt, &t.UpdatedAt, &lastObserved,
	); err != nil {
		return nil, err
	}
	if launchID.Valid {
		t.LaunchID = &launchID.Int64
	}
	if gpuClass.Valid {
		t.GPUClass = gpuClass.String
	}
	t.Cordoned = cordoned != 0
	if reason.Valid {
		t.CordonReason = reason.String
	}
	if cordonedAt.Valid {
		t.CordonedAt = &cordonedAt.Int64
	}
	if deadline.Valid {
		t.DeadlineUnix = &deadline.Int64
	}
	if currentJob.Valid {
		t.CurrentJobID = &currentJob.Int64
	}
	if lastObserved.Valid {
		t.LastObservedAt = &lastObserved.Int64
	}
	return &t, nil
}
