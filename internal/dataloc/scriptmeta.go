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
	GPU          string // GPU constraint (e.g., "nvidia", "ampere+", "a100")
	GPUClass     string // GPU class/generation
	GPUMemGB     int    // Requested GPU memory in GB (headroom may be applied by CLI)
	GPUMemStrict *bool  // Exact gpu-mem matching (no headroom), when explicitly set
	Inputs       []string
	Outputs      []string
	Tags         []string // Job tags
	Image        string   // Docker image override (e.g., "pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime")
	UvArgs       []string // Extra arguments to inject into `uv run` commands (e.g., ["--system"])
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

	weftTree := tree.Get("tool.weft")
	if weftTree == nil {
		return nil, nil
	}

	wt, ok := weftTree.(*toml.Tree)
	if !ok {
		return nil, nil
	}

	meta := &ScriptMeta{}

	if v, ok := wt.Get("gpu").(string); ok {
		meta.GPU = v
	}
	if v, ok := wt.Get("gpu-class").(string); ok {
		meta.GPUClass = v
	}
	meta.GPUMemGB = parseGPUMem(wt.Get("gpu-mem"))
	if v, ok := wt.Get("gpu-mem-strict").(bool); ok {
		meta.GPUMemStrict = &v
	}
	meta.Inputs = tomlStringSlice(wt, "inputs")
	meta.Outputs = tomlStringSlice(wt, "outputs")
	meta.Tags = tomlStringSlice(wt, "tags")
	if v, ok := wt.Get("image").(string); ok {
		meta.Image = v
	}
	meta.UvArgs = tomlStringSlice(wt, "uv-args")

	if meta.GPU == "" && meta.GPUClass == "" && meta.GPUMemGB == 0 && meta.GPUMemStrict == nil &&
		len(meta.Inputs) == 0 && len(meta.Outputs) == 0 && len(meta.Tags) == 0 && meta.Image == "" && len(meta.UvArgs) == 0 {
		return nil, nil
	}

	return meta, nil
}

// ScanScriptMeta extracts PEP 723 [tool.weft] metadata from the first Python
// script referenced in a shell command. Returns nil if no script is found or
// the script has no weft metadata.
func ScanScriptMeta(dir, command string) (*ScriptMeta, error) {
	scripts := extractPythonScripts(command)
	for _, script := range scripts {
		abs := script
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(dir, abs)
		}
		content, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		meta, err := ParseScriptMeta(string(content))
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", script, err)
		}
		if meta != nil {
			return meta, nil
		}
	}
	return nil, nil
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
