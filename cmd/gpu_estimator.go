package cmd

import (
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/gpucatalog"
	"github.com/osteele/weft/internal/placement"
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
	// When the job names a specific small GPU class, cap the blanket default
	// reservation to that card's VRAM so "--gpu t4" (16GB) doesn't inherit the
	// 20GB default that no T4 offer can satisfy. Try the dedicated class first,
	// then the raw --gpu spec (a family/index/family+mem spec has no single
	// ceiling and resolves to 0).
	classCeilingGB, ok := gpucatalog.MaxHardwareMemGB(gpuClass)
	if !ok {
		classCeilingGB, _ = gpucatalog.MaxHardwareMemGB(gpu)
	}
	floor, _, predicted = predictor.ResolveGPUMem(pcfg, explicit, needsGPU, host, project, gpuClass, command, defaultGPUMemGB, oomFloorGB, classCeilingGB)
	if floor != nil {
		effective := applyGPUMemHeadroom(*floor, explicit != nil, strict, false, gpuClass)
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

// applyGPUMemHeadroom returns the gpu_mem value to persist for the job.
// The +2GB safety headroom is skipped when:
//   - the value was not user-specified (e.g. a predicted floor),
//   - strict mode is on,
//   - the caller marks the value as already being a hardware floor, or
//   - (class, memGB) names a known hardware ceiling (gpucatalog.KnownHardwareMemoryGB) —
//     adding headroom to "A100 80GB" pushes the request above the hardware's
//     own gpu_ram and excludes every matching offer. The filter-time
//     EffectiveMemGB resolves the same condition for historical jobs, so
//     persisted values are sanitized regardless of which code path created
//     them.
func applyGPUMemHeadroom(memGB int, hasExplicitRequest bool, strict bool, hardwareFloor bool, gpuClass string) int {
	if !hasExplicitRequest || strict || hardwareFloor || memGB <= 0 {
		return memGB
	}
	if explicitGPUMemHardwareFloor(gpuClass, memGB) {
		return memGB
	}
	return memGB + defaultGPUMemHeadroomGB
}

func explicitGPUMemHardwareFloor(gpuClass string, memGB int) bool {
	return gpucatalog.KnownHardwareMemoryGB(gpuClass, memGB) || floatableHardwareMemoryFloor(gpuClass, memGB)
}

func floatableHardwareMemoryFloor(gpuClass string, memGB int) bool {
	if gpuClass == "" || memGB <= 0 {
		return false
	}
	return placement.ParseGPUConstraint(gpuClass).IsFloatable() && gpucatalog.KnownLargeHardwareMemoryGB(memGB)
}
