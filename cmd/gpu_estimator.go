package cmd

import (
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/predictor"
)

var loadPredictorConfig = config.Load

const defaultGPUMemHeadroomGB = 2

func resolveEffectiveGPUMemWithConfig(cfg *config.Config, explicit *int, gpu string, gpuClass string, strict bool, host string, project string, command string, oomFloorGB int) (*int, bool) {
	floor, _, predicted := resolveEffectiveGPUMemAndCeiling(cfg, explicit, gpu, gpuClass, strict, host, project, command, oomFloorGB)
	return floor, predicted
}

// resolveEffectiveGPUMemAndCeiling returns the GPU memory floor, ceiling, and
// whether the floor was predicted. The ceiling is nil when no prediction is
// available or the prediction exceeds all known VRAM tiers.
func resolveEffectiveGPUMemAndCeiling(cfg *config.Config, explicit *int, gpu string, gpuClass string, strict bool, host string, project string, command string, oomFloorGB int) (floor *int, ceiling *int, predicted bool) {
	pcfg := predictor.Config{}
	if cfg != nil {
		pcfg = buildPredictorConfig(cfg)
	}
	needsGPU := gpu != "" || gpuClass != ""
	floor, ceiling, predicted = predictor.ResolveGPUMem(pcfg, explicit, needsGPU, host, project, gpuClass, command, defaultGPUMemGB, oomFloorGB)
	if floor != nil {
		effective := applyGPUMemHeadroom(*floor, explicit != nil, strict)
		if effective != *floor {
			floor = &effective
			predicted = false
		}
	}
	if floor != nil && ceiling != nil && *ceiling < *floor {
		adjusted := *floor
		ceiling = &adjusted
	}
	return floor, ceiling, predicted
}

func resolveEffectiveGPUMem(explicit *int, gpu string, gpuClass string, host string, project string, command string) (*int, bool) {
	cfg, _ := loadPredictorConfig()
	return resolveEffectiveGPUMemWithConfig(cfg, explicit, gpu, gpuClass, false, host, project, command, 0)
}

func applyGPUMemHeadroom(memGB int, hasExplicitRequest bool, strict bool) int {
	if !hasExplicitRequest || strict || memGB <= 0 {
		return memGB
	}
	return memGB + defaultGPUMemHeadroomGB
}
