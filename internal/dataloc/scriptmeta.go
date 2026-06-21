package dataloc

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	toml "github.com/pelletier/go-toml"
)

// ScriptMeta holds weft resource requirements parsed from a PEP 723
// inline metadata block ([tool.weft] table).
type ScriptMeta struct {
	GPU           string // GPU constraint (e.g., "nvidia", "ampere+", "a100")
	GPUClass      string // GPU class/generation
	GPUCount      int    // Exact number of GPUs requested on one host
	GPUMemGB      int    // Requested GPU memory in GB (headroom may be applied by CLI)
	GPUMemStrict  *bool  // Exact gpu-mem matching (no headroom), when explicitly set
	Interconnect  string // Requested intra-host interconnect: any, pcie, nvlink
	CPUCores      int    // Minimum effective CPU cores/vCPUs
	CPUMemGB      int    // Minimum host/system RAM in GB (headroom may be applied by CLI)
	CPUMemStrict  *bool  // Exact cpu-mem matching (no headroom), when explicitly set
	DiskGB        int    // Total rental disk floor in GB
	RuntimeDiskGB int    // Extra scratch/cache disk headroom in GB
	// GPUArchMax bounds the GPU's CUDA compute capability from above. Accepted
	// values: a numeric cap ("9.0", "12.0"), a generation name ("ampere",
	// "hopper", "blackwell"), or "any" to disable inferred filtering. Empty
	// string means "auto-infer from the project's torch pin".
	GPUArchMax      string
	Inputs          []string
	Outputs         []string
	Tags            []string          // Job tags
	Image           string            // Docker image override (e.g., "pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime")
	MinDriver       string            // Minimum NVIDIA driver version for the image (e.g., "535")
	MinCUDA         string            // Minimum CUDA compatibility for the provider (e.g., "12.8")
	ImagePullSecret string            // Registry config key for pulling private images
	VastCapAdd      []string          // Vast.ai-only --cap-add values (e.g., ["SYS_ADMIN"])
	UvArgs          []string          // Extra arguments to inject into `uv run` commands (e.g., ["--system"])
	Env             map[string]string // Environment variables to set when running the job
	PreInstall      string            // Shell command to run before the job (e.g., "apt-get install -y libnuma-dev")
	Isolated        bool              // Skip project-level uv sync; script runs in an isolated PEP 723 environment
	Preemptible     bool              // Allow interruptible cloud placement (also accepts legacy "preemptible" key)
	HFOffline       *bool             // Run the job with HF_HUB_OFFLINE=1 (declared inputs are pre-staged), when explicitly set
}

// isEmpty reports whether the [tool.weft] block carries any user intent.
// Dependencies are intentionally excluded: they come from PEP 723's standard
// top-level `dependencies = [...]` field, not from [tool.weft], so a script
// that only declares standard PEP 723 deps must not look like "the user
// configured weft." Callers that use `meta == nil` as a proxy for "no
// [tool.weft] block" (e.g. cmd/restart.go applyScriptGPUDefaults) rely on
// this; counting Dependencies here would cause restart to clear GPU fields
// for any deps-only script.
func (m *ScriptMeta) isEmpty() bool {
	return m.GPU == "" && m.GPUClass == "" && m.GPUCount == 0 && m.GPUMemGB == 0 && m.GPUMemStrict == nil &&
		m.Interconnect == "" && m.CPUCores == 0 && m.CPUMemGB == 0 && m.CPUMemStrict == nil &&
		m.DiskGB == 0 && m.RuntimeDiskGB == 0 &&
		m.GPUArchMax == "" &&
		len(m.Inputs) == 0 && len(m.Outputs) == 0 && len(m.Tags) == 0 && m.Image == "" &&
		m.MinDriver == "" && m.MinCUDA == "" && m.ImagePullSecret == "" &&
		len(m.VastCapAdd) == 0 && len(m.UvArgs) == 0 && len(m.Env) == 0 &&
		m.PreInstall == "" && !m.Isolated && !m.Preemptible && m.HFOffline == nil
}

var (
	pep723StartRe = regexp.MustCompile(`^# /// script\s*$`)
	pep723EndRe   = regexp.MustCompile(`^# ///\s*$`)
	pythonExecRe  = regexp.MustCompile(`^python([0-9]+(\.[0-9]+)?)?$`)
)

// ParseScriptMeta extracts [tool.weft] from a PEP 723 inline metadata block.
// Returns nil if no metadata block or no [tool.weft] table is found.
func ParseScriptMeta(content string) (*ScriptMeta, error) {
	block := extractPEP723Block(content)
	if block == "" {
		return nil, nil
	}

	tree, err := toml.Load(block)
	if err != nil {
		return nil, fmt.Errorf("parse script metadata TOML: %w", err)
	}

	meta := &ScriptMeta{}

	// Parse [tool.weft] settings.
	if weftTree := tree.Get("tool.weft"); weftTree != nil {
		if wt, ok := weftTree.(*toml.Tree); ok {
			if v, ok := wt.Get("gpu").(string); ok {
				meta.GPU = v
			}
			if v, ok := wt.Get("gpu-class").(string); ok {
				meta.GPUClass = v
			}
			meta.GPUCount = parsePositiveInt(firstPresent(wt, "gpus", "gpu-count", "gpu_count"))
			meta.GPUMemGB = parseGPUMem(wt.Get("gpu-mem"))
			if v, ok := wt.Get("gpu-mem-strict").(bool); ok {
				meta.GPUMemStrict = &v
			}
			if v, ok := wt.Get("interconnect").(string); ok {
				meta.Interconnect = strings.TrimSpace(v)
			}
			meta.CPUCores = parsePositiveInt(firstPresent(wt, "cpu-cores", "cpu_cores"))
			meta.CPUMemGB = parseDiskGB(firstPresent(wt, "cpu-mem", "cpu-mem-gb", "cpu_mem_gb"))
			if v, ok := wt.Get("cpu-mem-strict").(bool); ok {
				meta.CPUMemStrict = &v
			}
			meta.DiskGB = parseDiskGB(firstPresent(wt, "disk", "disk-gb", "disk_gb"))
			meta.RuntimeDiskGB = parseDiskGB(firstPresent(wt, "runtime-disk", "runtime-disk-gb", "runtime_disk_gb"))
			if v, ok := wt.Get("gpu-arch-max").(string); ok {
				meta.GPUArchMax = v
			}
			meta.Inputs = tomlStringSlice(wt, "inputs")
			meta.Outputs = tomlStringSlice(wt, "outputs")
			meta.Tags = tomlStringSlice(wt, "tags")
			if v, ok := wt.Get("image").(string); ok {
				meta.Image = v
			}
			meta.MinDriver = firstStringValue(wt, "min-driver", "min_driver")
			// `cuda-driver-min` is the canonical key (matches the CLI flag);
			// `min-cuda` / `min_cuda` are accepted as legacy synonyms.
			meta.MinCUDA = firstStringValue(wt, "cuda-driver-min", "cuda_driver_min", "min-cuda", "min_cuda")
			meta.ImagePullSecret = firstStringValue(wt, "image-pull-secret", "image_pull_secret")
			meta.VastCapAdd = normalizeCaps(tomlStringSlice(wt, "vast-cap-add"))
			meta.UvArgs = tomlStringSlice(wt, "uv-args")
			meta.Env = tomlStringMap(wt, "env")
			if v, ok := wt.Get("pre-install").(string); ok {
				meta.PreInstall = v
			}
			if v, ok := wt.Get("isolated").(bool); ok {
				meta.Isolated = v
			}
			if v, ok := wt.Get("preemptible").(bool); ok {
				meta.Preemptible = v
			}
			if v, ok := wt.Get("interruptible").(bool); ok {
				meta.Preemptible = v
			}
			if v, ok := wt.Get("hf-offline").(bool); ok {
				meta.HFOffline = &v
			}
		}
	}

	// Parse [tool.uv] settings and convert to environment variables.
	// These have lower priority than explicit [tool.weft.env] entries.
	if uvTree := tree.Get("tool.uv"); uvTree != nil {
		if ut, ok := uvTree.(*toml.Tree); ok {
			uvEnv := extractUvEnvVars(ut)
			if len(uvEnv) > 0 {
				if meta.Env == nil {
					meta.Env = uvEnv
				} else {
					// [tool.weft.env] takes precedence over [tool.uv]
					for k, v := range uvEnv {
						if _, exists := meta.Env[k]; !exists {
							meta.Env[k] = v
						}
					}
				}
			}
		}
	}

	if meta.isEmpty() {
		return nil, nil
	}

	return meta, nil
}

func firstStringValue(tree *toml.Tree, keys ...string) string {
	for _, key := range keys {
		if v, ok := tree.Get(key).(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstPresent(tree *toml.Tree, keys ...string) interface{} {
	for _, key := range keys {
		if v := tree.Get(key); v != nil {
			return v
		}
	}
	return nil
}

// ScanScriptMeta extracts PEP 723 [tool.weft] metadata from the first Python
// script referenced in a shell command. Returns nil if no script is found or
// the script has no weft metadata.
func ScanScriptMeta(dir, command string) (*ScriptMeta, error) {
	content := readFirstPythonScript(dir, command)
	if content == "" {
		return nil, nil
	}
	return ParseScriptMeta(content)
}

// ScanScriptDependencies returns the PEP 723 standard top-level
// `dependencies = [...]` list, merged across EVERY Python script referenced
// in the shell command. Reading every script (not just the first) catches
// composite commands like `python preprocess.py && python serve.py` where
// only a later script declares the deps that determine the CUDA floor.
// Unlike ScanScriptMeta this does NOT short-circuit when the [tool.weft]
// block is empty — callers (e.g. ResolveJobImageSettings) specifically want
// the standard PEP 723 deps for CUDA-floor inference even when the script
// declares no weft-specific settings.
func ScanScriptDependencies(dir, command string) []string {
	var out []string
	for _, script := range ExtractPythonScriptsInDir(dir, command) {
		abs := resolveScriptPath(dir, command, script)
		content, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		block := extractPEP723Block(string(content))
		if block == "" {
			continue
		}
		tree, err := toml.Load(block)
		if err != nil {
			continue
		}
		deps, ok := tree.Get("dependencies").([]interface{})
		if !ok {
			continue
		}
		for _, d := range deps {
			if s, ok := d.(string); ok {
				if t := strings.TrimSpace(s); t != "" {
					out = append(out, t)
				}
			}
		}
	}
	return out
}

// readFirstPythonScript reads and returns the content of the first readable
// .py file referenced in a shell command. Returns "" if none is found.
func readFirstPythonScript(dir, command string) string {
	for _, script := range ExtractPythonScriptsInDir(dir, command) {
		abs := resolveScriptPath(dir, command, script)
		content, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		return string(content)
	}
	return ""
}

// extractPEP723Block returns the TOML content from a PEP 723 inline metadata
// block (between `# /// script` and `# ///`), stripping the `# ` prefix from
// each line.
func extractPEP723Block(content string) string {
	lines := strings.Split(content, "\n")
	var inBlock bool
	var blockLines []string

	for _, line := range lines {
		if !inBlock {
			if pep723StartRe.MatchString(line) {
				inBlock = true
			}
			continue
		}
		if pep723EndRe.MatchString(line) {
			break
		}
		// Strip "# " prefix (PEP 723 convention)
		if strings.HasPrefix(line, "# ") {
			blockLines = append(blockLines, line[2:])
		} else if line == "#" {
			blockLines = append(blockLines, "")
		}
	}

	if len(blockLines) == 0 {
		return ""
	}
	return strings.Join(blockLines, "\n")
}

// parseGPUMem interprets a gpu-mem value as an integer GB count.
// Accepts int64 or string formats like "40", "40GB", ">=80GB".
func parseGPUMem(v interface{}) int {
	return parseDiskGB(v)
}

func parsePositiveInt(v interface{}) int {
	switch val := v.(type) {
	case int64:
		if val > 0 {
			return int(val)
		}
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(val), "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// parseDiskGB interprets a disk size as an integer GB count.
// Accepts int64 or string formats like "80", "80GB", ">=80GB".
func parseDiskGB(v interface{}) int {
	switch val := v.(type) {
	case int64:
		return int(val)
	case string:
		s := strings.TrimPrefix(val, ">=")
		s = strings.TrimSuffix(strings.TrimSuffix(s, "GB"), "gb")
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err == nil {
			return n
		}
	}
	return 0
}

// InjectUvArgs inserts extra arguments into a "uv run" command.
// For example, InjectUvArgs("uv run script.py", {"--system"}) returns
// "uv run --system script.py".
func InjectUvArgs(command string, uvArgs []string) string {
	if len(uvArgs) == 0 {
		return command
	}
	before, after, ok := strings.Cut(command, "uv run")
	if !ok {
		return command
	}
	return before + "uv run " + strings.Join(uvArgs, " ") + after
}

// ApplyUvArgs applies uv-args to a command.
// Behavior:
//   - If command already contains "uv run", inject uvArgs into that invocation.
//   - Otherwise, rewrite common direct Python forms (python/python3[.N] script.py)
//     to an equivalent "uv run <uvArgs> ..." command.
//   - Compound shell commands are left unchanged.
func ApplyUvArgs(command string, uvArgs []string) string {
	if len(uvArgs) == 0 {
		return command
	}

	injected := InjectUvArgs(command, uvArgs)
	if injected != command {
		return injected
	}

	if hasShellOperators(command) {
		return command
	}

	return rewritePythonCommandWithUv(command, uvArgs)
}

func hasShellOperators(command string) bool {
	for _, op := range []string{"&&", "||", "|", ";"} {
		if strings.Contains(command, op) {
			return true
		}
	}
	return false
}

func rewritePythonCommandWithUv(command string, uvArgs []string) string {
	tokens := strings.Fields(command)
	if len(tokens) == 0 {
		return command
	}

	pythonIdx := -1
	for i, tok := range tokens {
		if isPythonExecToken(tok) {
			pythonIdx = i
			break
		}
		// Allow leading env var assignments.
		if strings.Contains(tok, "=") && !strings.HasPrefix(tok, "-") {
			continue
		}
		return command
	}
	if pythonIdx == -1 {
		return command
	}

	scriptIdx := -1
	for i := pythonIdx + 1; i < len(tokens); i++ {
		tok := tokens[i]
		if strings.HasSuffix(tok, ".py") && !strings.Contains(tok, "=") {
			scriptIdx = i
			break
		}
	}
	if scriptIdx == -1 {
		return command
	}

	uvPrefix := "uv run " + strings.Join(uvArgs, " ")
	pythonFlags := tokens[pythonIdx+1 : scriptIdx]
	scriptAndArgs := strings.Join(tokens[scriptIdx:], " ")

	var rewritten string
	if len(pythonFlags) == 0 {
		rewritten = uvPrefix + " " + scriptAndArgs
	} else {
		rewritten = uvPrefix + " python " + strings.Join(pythonFlags, " ") + " " + scriptAndArgs
	}

	if pythonIdx > 0 {
		return strings.Join(tokens[:pythonIdx], " ") + " " + rewritten
	}
	return rewritten
}

func isPythonExecToken(token string) bool {
	base := filepath.Base(token)
	return pythonExecRe.MatchString(base)
}

// extractUvEnvVars converts [tool.uv] settings to environment variables.
// Supported keys: index-url → UV_INDEX_URL, extra-index-url → UV_EXTRA_INDEX_URL.
func extractUvEnvVars(ut *toml.Tree) map[string]string {
	var env map[string]string
	setEnv := func(k, v string) {
		if env == nil {
			env = make(map[string]string)
		}
		env[k] = v
	}
	if v, ok := ut.Get("index-url").(string); ok && v != "" {
		setEnv("UV_INDEX_URL", v)
	}
	if urls := tomlStringOrSlice(ut, "extra-index-url"); len(urls) > 0 {
		setEnv("UV_EXTRA_INDEX_URL", strings.Join(urls, " "))
	}
	return env
}

// tomlStringOrSlice extracts a TOML value that can be either a single string
// or an array of strings.
func tomlStringOrSlice(tree *toml.Tree, key string) []string {
	v := tree.Get(key)
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	var result []string
	for _, item := range arr {
		if s, ok := item.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

func tomlStringSlice(tree *toml.Tree, key string) []string {
	v := tree.Get(key)
	if v == nil {
		return nil
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	var result []string
	for _, item := range arr {
		if s, ok := item.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

func tomlStringMap(tree *toml.Tree, key string) map[string]string {
	v := tree.Get(key)
	if v == nil {
		return nil
	}
	sub, ok := v.(*toml.Tree)
	if !ok {
		return nil
	}
	result := make(map[string]string, len(sub.Keys()))
	for _, k := range sub.Keys() {
		if s, ok := sub.Get(k).(string); ok {
			result[k] = s
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func normalizeCaps(caps []string) []string {
	if len(caps) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(caps))
	out := make([]string, 0, len(caps))
	for _, capVal := range caps {
		capVal = strings.ToUpper(strings.TrimSpace(capVal))
		if capVal == "" {
			continue
		}
		if _, ok := seen[capVal]; ok {
			continue
		}
		seen[capVal] = struct{}{}
		out = append(out, capVal)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
