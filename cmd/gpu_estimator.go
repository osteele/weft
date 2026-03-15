package cmd

import (
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/predictor"
)

func resolveEffectiveGPUMemWithConfig(cfg *config.Config, explicit *int, gpu string, gpuClass string, host string, project string, command string) (*int, bool) {
	pcfg := predictor.Config{}
	if cfg != nil {
		pcfg = buildPredictorConfig(cfg)
	}
	needsGPU := gpu != "" || gpuClass != ""
	return predictor.ResolveGPUMemGB(pcfg, explicit, needsGPU, host, project, gpuClass, command, defaultGPUMemGB)
}

func resolveEffectiveGPUMem(explicit *int, gpu string, gpuClass string, host string, project string, command string) (*int, bool) {
	cfg, _ := config.Load()
	return resolveEffectiveGPUMemWithConfig(cfg, explicit, gpu, gpuClass, host, project, command)
}
