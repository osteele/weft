package placement

import (
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/predictor"
)

// PredictorConfigFromApp builds a predictor.Config from the app config.
// Returns a zero Config if cfg is nil.
func PredictorConfigFromApp(cfg *config.Config) predictor.Config {
	if cfg == nil {
		return predictor.Config{}
	}
	pcfg := predictor.BuildConfig(
		cfg.Predictor.ProjectPath,
		cfg.Predictor.ModelDir,
		cfg.Predictor.RetrainInterval,
		cfg.Predictor.DBPaths,
	)
	pcfg.Enabled = cfg.Predictor.Enabled
	return pcfg
}

// BuildJobPredictorFromConfig creates a JobPredictor from the app config and constraints.
// Returns nil if the predictor is not configured or no command is set.
func BuildJobPredictorFromConfig(cfg *config.Config, c Constraints) JobPredictor {
	pcfg := PredictorConfigFromApp(cfg)
	if !pcfg.Configured() || c.Command == "" {
		return nil
	}
	return NewJobPredictor(func(host string) *RawPrediction {
		result, err := predictor.ResolvePredict(pcfg, host, c.Project, c.GPUClass, c.Command)
		if err != nil {
			return nil
		}
		raw := &RawPrediction{}
		if result.DurationS != nil {
			raw.DurationS = &RawPredictionField{
				Mean:            result.DurationS.Mean,
				Lower:           result.DurationS.Lower,
				Upper:           result.DurationS.Upper,
				EpistemicFactor: result.DurationS.EpistemicFactor,
				NCalibration:    result.DurationS.NCalibration,
			}
			if result.DurationMetadata != nil {
				raw.OODReasons = append([]string(nil), result.DurationMetadata.OODReasons...)
			}
		}
		if result.PeakRSSKB != nil {
			raw.PeakRSSKB = &RawPredictionField{Mean: result.PeakRSSKB.Mean, Lower: result.PeakRSSKB.Lower, Upper: result.PeakRSSKB.Upper}
		}
		if result.MaxGPUMemMiB != nil {
			raw.MaxGPUMemMiB = &RawPredictionField{Mean: result.MaxGPUMemMiB.Mean, Lower: result.MaxGPUMemMiB.Lower, Upper: result.MaxGPUMemMiB.Upper}
		}
		return raw
	})
}
