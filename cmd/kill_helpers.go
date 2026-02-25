package cmd

import (
	"github.com/osteele/weft/internal/core"
	"github.com/osteele/weft/internal/ops"
)

// killJobWithService kills or cancels a job using the shared core service. When
// svc is nil the helper opens a temporary service so callers like --kill can
// reuse this logic without wiring their own database access.
func killJobWithService(svc *core.Service, jobID int64, mode ops.TimeoutMode) (core.OperationResult, error) {
	if svc != nil {
		return svc.KillJob(jobID, mode)
	}
	temp, err := core.NewService()
	if err != nil {
		return core.OperationResult{}, err
	}
	defer temp.Close()
	return temp.KillJob(jobID, mode)
}
