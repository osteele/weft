package placement

import (
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
)

const (
	SGLangRuntimeImage        = "lmsysorg/sglang:v0.5.10.post1"
	SGLangDevCU13RuntimeImage = "lmsysorg/sglang:dev-cu13"
	LegacySGLangRuntimeImage  = "ghcr.io/osteele/sglang-runtime:v0.5.10.post1"
)

type RuntimeFloorMode int

const (
	RuntimeFloorFamily RuntimeFloorMode = iota
	RuntimeFloorExact
)

type RuntimeImageSpec struct {
	Aliases        []string
	CanonicalImage string
	MinCUDA        string
	MinDriver      int
	OwnsTorch      bool
}

type EffectiveRuntimeOptions struct {
	FloorMode RuntimeFloorMode
}

type EffectiveRuntime struct {
	Image           string
	ImagePullSecret string
	Floor           RuntimeFloor
}

var runtimeImageSpecs = []RuntimeImageSpec{
	{
		Aliases:        []string{"sglang", "sglang:0.5.10", "sglang:v0.5.10.post1", LegacySGLangRuntimeImage},
		CanonicalImage: SGLangRuntimeImage,
		OwnsTorch:      true,
	},
	{
		Aliases:        []string{"sglang:dev-cu13", "sglang:fp4", "sglang:fp4-e2m1"},
		CanonicalImage: SGLangDevCU13RuntimeImage,
		MinCUDA:        "13.0",
		MinDriver:      580,
		OwnsTorch:      true,
	},
}

func NormalizeRuntimeImageAlias(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return ""
	}
	lower := strings.ToLower(image)
	for _, spec := range runtimeImageSpecs {
		for _, alias := range spec.Aliases {
			if lower == strings.ToLower(alias) {
				return spec.CanonicalImage
			}
		}
	}
	if strings.HasPrefix(lower, "ghcr.io/osteele/sglang-runtime:") {
		return SGLangRuntimeImage
	}
	return image
}

func RuntimeImageOwnsTorch(image string) bool {
	image = NormalizeRuntimeImageAlias(image)
	for _, spec := range runtimeImageSpecs {
		if image == spec.CanonicalImage {
			return spec.OwnsTorch
		}
	}
	return false
}

func SGLangRuntimeImageForJob(localDir, command string) string {
	if jobUsesSGLangFP4KV(localDir, command) {
		return SGLangDevCU13RuntimeImage
	}
	return SGLangRuntimeImage
}

func ResolveEffectiveRuntime(localDir, command string, opts EffectiveRuntimeOptions) (EffectiveRuntime, error) {
	var projectCloud config.ProjectCloudConfig
	var projectDir string
	if cfg, cfgPath, err := config.LoadProjectConfigWithPath(localDir); err != nil {
		slog.Warn("error loading project config for runtime resolution", "component", "placement", "dir", localDir, "error", err)
	} else if cfg != nil {
		projectCloud = cfg.Cloud
		projectDir = filepath.Dir(cfgPath)
	}

	img := strings.TrimSpace(projectCloud.Image)
	if override := resolveProjectImageOverride(projectCloud.ImageOverrides, projectDir, localDir, command); override != "" {
		img = override
	}
	if img == "" && JobUsesFramework(localDir, command, "sglang") {
		img = SGLangRuntimeImageForJob(localDir, command)
	}
	imagePullSecret := projectCloud.ImagePullSecret

	meta, metaErr := dataloc.ScanScriptMeta(localDir, command)
	if metaErr != nil {
		slog.Warn("script metadata error in runtime resolution", "component", "placement", "error", metaErr)
	} else if meta != nil {
		if meta.Image != "" {
			img = meta.Image
		}
		if imagePullSecret == "" {
			imagePullSecret = meta.ImagePullSecret
		}
	}
	img = NormalizeRuntimeImageAlias(img)

	rf := resolveRuntimeFloor(localDir, command, opts.FloorMode)
	for _, spec := range runtimeImageSpecs {
		if img != spec.CanonicalImage {
			continue
		}
		req := cloud.ImageRequirements{MinCUDAVersion: spec.MinCUDA, MinDriverVersion: spec.MinDriver}
		rf.MergeInferred(req, fmt.Sprintf("%s runtime image", spec.CanonicalImage))
		break
	}

	var parseErr error
	if err := rf.ApplyExplicit(projectCloud.MinDriver, projectCloud.MinCUDA, ".weft.toml [cloud]"); err != nil {
		parseErr = err
	}
	if meta != nil {
		if err := rf.ApplyExplicit(meta.MinDriver, meta.MinCUDA, "script [tool.weft]"); err != nil {
			parseErr = err
		}
	}
	rf.FinalizeDriver()
	return EffectiveRuntime{Image: img, ImagePullSecret: imagePullSecret, Floor: rf}, parseErr
}

func resolveRuntimeFloor(dir, command string, mode RuntimeFloorMode) RuntimeFloor {
	var rf RuntimeFloor
	if scriptReq := dataloc.ScanScriptTorchRequirement(dir, command); scriptReq != nil {
		if mode == RuntimeFloorExact {
			if scriptCUDA := dataloc.ScriptTorchCUDAVersion(dir, command); scriptCUDA != "" {
				origin := "script PEP 723 torch dependency"
				if !scriptReq.Exact {
					origin = "script PEP 723 torch open range"
				}
				rf.MergeInferred(cloud.ImageRequirements{MinCUDAVersion: scriptCUDA}, origin)
			}
		} else {
			mergeScriptTorchRuntimeFloor(&rf, scriptReq, dataloc.ScanScriptTorchPin(dir, command))
		}
	} else if pin := dataloc.ScanTorchPin(dir); pin != nil {
		if mode == RuntimeFloorExact {
			if cuda := dataloc.CUDAVariantVersion(pin.CudaVariant); cuda != "" {
				rf.MergeInferred(cloud.ImageRequirements{MinCUDAVersion: cuda}, "torch pin")
			}
		} else {
			mergeProjectTorchRuntimeFloor(&rf, pin)
		}
	}

	deps := append([]dataloc.DepSpec{}, dataloc.ScanUVRunWith(command)...)
	deps = append(deps, dataloc.ParseDepSpecs(dataloc.ScanScriptDependencies(dir, command))...)
	if libCUDA := dataloc.LibraryMinCUDAFromDeps(deps); libCUDA != "" {
		const origin = "library dependency CUDA floor"
		before := rf.Req.MinCUDAVersion
		rf.MergeInferred(cloud.ImageRequirements{MinCUDAVersion: libCUDA}, origin)
		if mode == RuntimeFloorFamily && rf.Req.MinCUDAVersion == before && rf.Req.MinCUDAVersion == libCUDA {
			rf.CUDAOrigin = origin
			rf.InferredCUDAOrigin = origin
		}
	}
	return rf
}

func mergeProjectTorchRuntimeFloor(rf *RuntimeFloor, pin *dataloc.TorchPin) {
	if pin == nil {
		return
	}
	if family := dataloc.CUDAFamilyFloor(pin.CudaVariant); family != "" {
		major, _, _ := strings.Cut(family, ".")
		origin := fmt.Sprintf("torch %s+%s (CUDA %s.x family)", pin.Version, pin.CudaVariant, major)
		rf.MergeInferred(cloud.ImageRequirements{MinCUDAVersion: family}, origin)
	}
	if cuda, origin := torchOperationalCUDAFloor(pin); cuda != "" {
		rf.MergeInferred(cloud.ImageRequirements{MinCUDAVersion: cuda}, origin)
	}
}

func JobUsesFramework(localDir, command, framework string) bool {
	framework = strings.ToLower(framework)
	if strings.Contains(strings.ToLower(command), framework) {
		return true
	}
	if localDir != "" {
		if fileContains(filepath.Join(localDir, "pyproject.toml"), framework) {
			return true
		}
		for _, script := range dataloc.ExtractPythonScriptsInDir(localDir, command) {
			if !filepath.IsAbs(script) {
				script = filepath.Join(localDir, script)
			}
			if fileContains(script, framework) {
				return true
			}
		}
	}
	return false
}

func jobUsesSGLangFP4KV(localDir, command string) bool {
	lowerCommand := strings.ToLower(command)
	if strings.Contains(lowerCommand, "fp4_e2m1") || strings.Contains(lowerCommand, "kv_cache_dtype=fp4") {
		return true
	}
	if localDir == "" {
		return false
	}
	for _, script := range dataloc.ExtractPythonScriptsInDir(localDir, command) {
		if !filepath.IsAbs(script) {
			script = filepath.Join(localDir, script)
		}
		if fileContains(script, "fp4_e2m1") || fileContains(script, "kv_cache_dtype=\"fp4") || fileContains(script, "kv_cache_dtype='fp4") {
			return true
		}
	}
	return false
}

func fileContains(filePath, needle string) bool {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(data)), needle)
}

func resolveProjectImageOverride(overrides map[string]string, projectDir, localDir, command string) string {
	if len(overrides) == 0 || projectDir == "" {
		return ""
	}
	scripts := dataloc.ExtractPythonScriptsInDir(localDir, command)
	if len(scripts) == 0 {
		return ""
	}

	script := scripts[0]
	if !filepath.IsAbs(script) {
		script = filepath.Join(localDir, script)
	}
	rel, err := filepath.Rel(projectDir, script)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return ""
	}
	rel = filepath.ToSlash(filepath.Clean(rel))

	var bestPattern, bestImage string
	for pattern, image := range overrides {
		image = strings.TrimSpace(image)
		if image == "" {
			continue
		}
		normalizedPattern := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(pattern)), "./")
		matched, err := path.Match(normalizedPattern, rel)
		if err != nil {
			slog.Warn("invalid cloud image override pattern", "component", "placement", "pattern", pattern, "error", err)
			continue
		}
		if !matched {
			continue
		}
		if bestPattern == "" || len(normalizedPattern) > len(bestPattern) || (len(normalizedPattern) == len(bestPattern) && normalizedPattern < bestPattern) {
			bestPattern = normalizedPattern
			bestImage = image
		}
	}
	return bestImage
}
