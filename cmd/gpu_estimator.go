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

// resolveEffectiveGPUMemAndCeiling returns the GPU memory floor and whether the
// floor was predicted. The second return value is legacy compatibility metadata;
// new submissions do not derive a maximum GPU memory requirement from predictor
// telemetry.
func resolveEffectiveGPUMemAndCeiling(cfg *config.Config, explicit *int, gpu string, gpuClass string, strict bool, host string, project string, command string, oomFloorGB int) (floor *int, ceiling *int, predicted bool) {
	pcfg := predictor.Config{}
	if cfg != nil {
		pcfg = buildPredictorConfig(cfg)
	}
	needsGPU := gpu != "" || gpuClass != ""
	floor, _, predicted = predictor.ResolveGPUMem(pcfg, explicit, needsGPU, host, project, gpuClass, command, defaultGPUMemGB, oomFloorGB)
	if floor != nil {
		effective := applyGPUMemHeadroom(*floor, explicit != nil, strict)
		if effective != *floor {
			floor = &effective
			predicted = false
		}
	}
	return floor, nil, predicted
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
