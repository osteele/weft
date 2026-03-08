package estimate

import (
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/predictor"
)

// DefaultJobDuration is the fallback when no prediction is available.
var DefaultJobDuration = Estimate{
	Mean:  1 * time.Hour,
	Lower: 15 * time.Minute,
	Upper: 4 * time.Hour,
}

// EstimateJobDuration wraps the ML predictor for a single job.
// Returns (estimate, true) when a prediction is available, or
// (DefaultJobDuration, false) as a fallback.
func EstimateJobDuration(predCfg *predictor.Config, gpuClass string, job *db.Job) (Estimate, bool) {
	if predCfg == nil || !predCfg.Configured() {
		return DefaultJobDuration, false
	}

	result, err := predictor.Predict(*predCfg, "", job.Project, gpuClass, job.Command)
	if err != nil || result == nil || result.DurationS == nil {
		return DefaultJobDuration, false
	}

	return FromSeconds(result.DurationS.Mean, result.DurationS.Lower, result.DurationS.Upper), true
}

// EstimateJobDurations predicts durations for multiple jobs in a single
// subprocess call. Returns a map from job ID to Estimate, or nil if the
// predictor is not configured or the batch call fails.
func EstimateJobDurations(predCfg *predictor.Config, batchJobs []predictor.BatchJob) map[int64]Estimate {
	if predCfg == nil || !predCfg.Configured() || len(batchJobs) == 0 {
		return nil
	}

	results, err := predictor.PredictBatch(*predCfg, batchJobs)
	if err != nil || results == nil {
		return nil
	}

	estimates := make(map[int64]Estimate, len(results))
	for id, r := range results {
		if r != nil && r.DurationS != nil {
			estimates[id] = FromSeconds(r.DurationS.Mean, r.DurationS.Lower, r.DurationS.Upper)
		}
	}
	return estimates
}

// EstimateCloudRuntime scales a local duration estimate by the ratio of GPU
// performance scores. localDLPerf and cloudDLPerf are deep-learning benchmark
// scores; higher is faster.
func EstimateCloudRuntime(localEst Estimate, localDLPerf, cloudDLPerf float64) Estimate {
	if localDLPerf <= 0 || cloudDLPerf <= 0 {
		return localEst
	}
	return localEst.Scale(localDLPerf / cloudDLPerf)
}
