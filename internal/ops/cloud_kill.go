package ops

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/session"
)

// KillCloudJob kills a cloud-hosted job by SSHing into the cloud instance.
// If inst is nil or terminal, it only updates the local DB status.
// Returns true if the instance was already terminal (for caller messaging).
func KillCloudJob(database *sql.DB, job *db.Job, inst *db.Launch, cloudClient cloud.Client, timeout time.Duration) (wasTerminal bool, err error) {
	if !job.IsLaunchJob() {
		return false, fmt.Errorf("job %d is not a cloud job", job.ID)
	}

	// Instance record missing or terminal — just update DB
	if inst == nil || inst.IsTerminal() {
		return inst != nil && inst.IsTerminal(),
			db.UpdateStatusAndLastSynced(database, job.ID, db.StatusKilled)
	}

	// Instance is running or in grace — SSH in and kill the process
	providerID := inst.EffectiveProviderID()
	cloudInst, err := cloudClient.ShowInstance(providerID)
	if err != nil {
		// Instance gone from provider — update DB
		return true, db.UpdateStatusAndLastSynced(database, job.ID, db.StatusKilled)
	}

	// Build the kill command (same PGID/PID logic as killQueueRunnerJob)
	pgidFile := session.SimplePgidFile(job.ID)
	killReasonFile := session.SimpleKillReasonFile(job.ID)
	killCmd := fmt.Sprintf(`
		echo "user_kill" > %s
		if [ -f %s ]; then
			pgid=$(cat %s 2>/dev/null)
			if [ -n "$pgid" ]; then
				kill -TERM -$pgid 2>/dev/null || true
				sleep 0.5
				kill -KILL -$pgid 2>/dev/null || true
			fi
			rm -f %s
		fi
	`, killReasonFile, pgidFile, pgidFile, pgidFile)

	if _, err := cloud.RunOnInstance(cloudInst, killCmd, timeout); err != nil {
		return false, fmt.Errorf("kill cloud job process: %w", err)
	}

	return false, db.UpdateStatusAndLastSynced(database, job.ID, db.StatusKilled)
}
