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
	Estimate  Estimate
	Metadata  *predictor.RuntimeMetadata
	Quantiles *predictor.QuantilePrediction
}

// DecisionDurationQuantiles is the bounded grid Weft asks job-estimator to
// return for downstream asymmetric-cost decisions. Consumers interpolate when
// their exact critical fractile falls between grid points.
func DecisionDurationQuantiles() []float64 {
	return []float64{0.05, 0.10, 0.20, 0.30, 0.40, 0.50, 0.60, 0.70, 0.80, 0.85, 0.90, 0.95}
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
	ApplyResidualCorrection(job.ID, job.Project, gpuClass, job.Command, result)

	return DurationPrediction{
		Estimate: FromSeconds(result.DurationS.Mean, result.DurationS.Lower, result.DurationS.Upper),
		Metadata: result.DurationMetadata,
	}, true
}

// EstimateJobDurationsDetailed predicts durations for multiple jobs in a single
// subprocess call and preserves runtime metadata for each prediction.
func EstimateJobDurationsDetailed(predCfg *predictor.Config, batchJobs []predictor.BatchJob) map[int64]DurationPrediction {
	return EstimateJobDurationsDetailedWithProgress(predCfg, batchJobs, nil)
}

// EstimateJobDurationsDetailedWithProgress is the progress-aware variant of
// EstimateJobDurationsDetailed. The progress callback, if non-nil, is invoked
// with short user-facing status strings as prediction advances (cache check,
// subprocess start, completion). Safe to call from the calling goroutine;
// nil progress is a no-op.
func EstimateJobDurationsDetailedWithProgress(predCfg *predictor.Config, batchJobs []predictor.BatchJob, progress func(string)) map[int64]DurationPrediction {
	if predCfg == nil || !predCfg.Configured() || len(batchJobs) == 0 {
		return nil
	}

	batchJobs = withDecisionDurationQuantiles(batchJobs)
	results, err := predictor.ResolvePredictBatchWithProgress(*predCfg, batchJobs, progress)
	if err != nil || results == nil {
		return nil
	}

	estimates := make(map[int64]DurationPrediction, len(results))
	for id, r := range results {
		if r != nil && r.DurationS != nil {
			if job := batchJobByID(batchJobs, id); job != nil {
				ApplyResidualCorrection(id, job.Project, job.GPUClass, job.Command, r)
			}
			estimates[id] = DurationPrediction{
				Estimate:  FromSeconds(r.DurationS.Mean, r.DurationS.Lower, r.DurationS.Upper),
				Metadata:  r.DurationMetadata,
				Quantiles: r.DurationQuantiles,
			}
		}
	}
	return estimates
}

func withDecisionDurationQuantiles(batchJobs []predictor.BatchJob) []predictor.BatchJob {
	out := append([]predictor.BatchJob(nil), batchJobs...)
	quantiles := DecisionDurationQuantiles()
	for i := range out {
		if len(out[i].DurationQuantiles) == 0 {
			out[i].DurationQuantiles = append([]float64(nil), quantiles...)
		}
	}
	return out
}

func batchJobByID(batchJobs []predictor.BatchJob, id int64) *predictor.BatchJob {
	for i := range batchJobs {
		if batchJobs[i].ID == id {
			return &batchJobs[i]
		}
	}
	return nil
}
