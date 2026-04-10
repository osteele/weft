package cmd

import (
	"database/sql"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/orchestration"
)

// attemptRelaunchOrphanedJobs resets jobs on terminal cloud instances and
// launches replacement instances for scoped unplaced cloud jobs. extraAttempts
// raises the max attempt threshold (used for manual retries to allow more tries).
func attemptRelaunchOrphanedJobs(
	database *sql.DB,
	cfg *config.Config,
	extraAttempts int,
	retryBudgetMultiplierByFailedInstance map[int64]float64,
	scopeJobIDs []int64,
	scopeProject string,
	restrictToReset bool,
	includeFreshUnplaced bool,
) (*campaign.RelaunchResult, error) {
	return orchestration.RelaunchOrphanedJobs(
		database,
		cfg,
		extraAttempts,
		retryBudgetMultiplierByFailedInstance,
		scopeJobIDs,
		scopeProject,
		restrictToReset,
		includeFreshUnplaced,
	)
}
