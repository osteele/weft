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

// DurationPrediction bundles a runtime estimate with estimator metadata.
type DurationPrediction struct {
	Estimate Estimate
	Metadata *predictor.RuntimeMetadata
}

// EstimateJobDurationDetailed returns a single-job prediction plus runtime metadata.
func EstimateJobDurationDetailed(predCfg *predictor.Config, gpuClass string, job *db.Job) (DurationPrediction, bool) {
	if predCfg == nil || !predCfg.Configured() {
		return DurationPrediction{Estimate: DefaultJobDuration}, false
	}

	result, err := predictor.Predict(*predCfg, "", job.Project, gpuClass, job.Command)
	if err != nil || result == nil || result.DurationS == nil {
		return DurationPrediction{Estimate: DefaultJobDuration}, false
	}

	return DurationPrediction{
		Estimate: FromSeconds(result.DurationS.Mean, result.DurationS.Lower, result.DurationS.Upper),
		Metadata: result.DurationMetadata,
	}, true
}

// EstimateJobDurationsDetailed predicts durations for multiple jobs in a single
// subprocess call and preserves runtime metadata for each prediction.
func EstimateJobDurationsDetailed(predCfg *predictor.Config, batchJobs []predictor.BatchJob) map[int64]DurationPrediction {
	if predCfg == nil || !predCfg.Configured() || len(batchJobs) == 0 {
		return nil
	}

	results, err := predictor.ResolvePredictBatch(*predCfg, batchJobs)
	if err != nil || results == nil {
		return nil
	}

	estimates := make(map[int64]DurationPrediction, len(results))
	for id, r := range results {
		if r != nil && r.DurationS != nil {
			estimates[id] = DurationPrediction{
				Estimate: FromSeconds(r.DurationS.Mean, r.DurationS.Lower, r.DurationS.Upper),
				Metadata: r.DurationMetadata,
			}
		}
	}
	return estimates
}
