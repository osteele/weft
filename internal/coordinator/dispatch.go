package coordinator

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/intent"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
)

// dispatchIntent creates a job record and appends it to the remote queue.
// Returns the job ID and any error.
func dispatchIntent(database *sql.DB, i *intent.Intent, host string) (int64, error) {
	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  i.Job.Dir,
		Command:     i.Job.Cmd,
		Description: i.Job.Desc,
		EnvVars:     i.Job.Env,
		Tags:        i.Job.Tags,
		GPUClass:    i.Job.Constraints.GPUClass,
		GPUMemGB:    i.Job.GPUMemGB,
		DepSpec:     i.Job.DepSpec,
		Inputs:      i.Job.Inputs,
		Outputs:     i.Job.Outputs,
	}

	result, err := ops.QueueJob(database, params, ops.DefaultOptions())
	if err != nil {
		return 0, fmt.Errorf("queue job on %s: %w", host, err)
	}
	return result.JobID, nil
}

// resolveHost determines which host should run the job described by the intent.
// If the intent specifies a host constraint, that host is used directly.
// Otherwise, placement scoring selects the best host.
func resolveHost(database *sql.DB, i *intent.Intent) (string, []string, error) {
	if i.Job.Constraints.Host != "" {
		return i.Job.Constraints.Host, []string{"explicit host"}, nil
	}

	constraints := placement.Constraints{
		GPUClass: i.Job.Constraints.GPUClass,
		GPUMemGB: i.Job.Constraints.GPUMemGB,
		Inputs:   i.Job.Inputs,
	}

	host, reasons, err := placement.BestHost(database, constraints)
	if err != nil {
		return "", nil, fmt.Errorf("placement: %w", err)
	}
	return host, reasons, nil
}
