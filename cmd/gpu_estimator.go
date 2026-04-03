package cmd

import (
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/predictor"
)

var loadPredictorConfig = config.Load

func resolveEffectiveGPUMemWithConfig(cfg *config.Config, explicit *int, gpu string, gpuClass string, host string, project string, command string, oomFloorGB int) (*int, bool) {
	floor, _, predicted := resolveEffectiveGPUMemAndCeiling(cfg, explicit, gpu, gpuClass, host, project, command, oomFloorGB)
	return floor, predicted
}

// resolveEffectiveGPUMemAndCeiling returns the GPU memory floor, ceiling, and
// whether the floor was predicted. The ceiling is nil when no prediction is
// available or the prediction exceeds all known VRAM tiers.
func resolveEffectiveGPUMemAndCeiling(cfg *config.Config, explicit *int, gpu string, gpuClass string, host string, project string, command string, oomFloorGB int) (floor *int, ceiling *int, predicted bool) {
	pcfg := predictor.Config{}
	if cfg != nil {
		pcfg = buildPredictorConfig(cfg)
	}
	needsGPU := gpu != "" || gpuClass != ""
	return predictor.ResolveGPUMem(pcfg, explicit, needsGPU, host, project, gpuClass, command, defaultGPUMemGB, oomFloorGB)
}

func resolveEffectiveGPUMem(explicit *int, gpu string, gpuClass string, host string, project string, command string) (*int, bool) {
	cfg, _ := loadPredictorConfig()
	return resolveEffectiveGPUMemWithConfig(cfg, explicit, gpu, gpuClass, host, project, command, 0)
}
