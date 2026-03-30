package remediation

import (
	"regexp"
	"strings"
)

// pattern is a compiled error pattern matcher.
type pattern struct {
	re             *regexp.Regexp
	patternID      string // e.g., "missing_hf_model"
	category       string // "data", "code", "environment"
	message        string // human-readable template
	remediable     bool
	fatalAtRuntime bool // if true, kill the job immediately when detected in live logs
	// extractAssets extracts data asset refs from regex match groups.
	// Only used for data patterns.
	extractAssets func(match []string) []string
}

// Match tests log content against this pattern. Returns a diagnosis if matched.
func (p *pattern) Match(logContent string) *ErrorDiagnosis {
	matches := p.re.FindStringSubmatch(logContent)
	if matches == nil {
		return nil
	}
	d := &ErrorDiagnosis{
		Pattern:    p.patternID,
		Category:   p.category,
		Message:    p.message,
		Remediable: p.remediable,
		Details:    matches[0],
	}
	if p.extractAssets != nil {
		d.MissingAssets = p.extractAssets(matches)
	}
	return d
}

// Data patterns: missing HF models/datasets, missing files (remediable)
var dataPatterns = []*pattern{
	{
		re:         regexp.MustCompile(`(?i)(?:FileNotFoundError|OSError|No such file or directory).*huggingface/hub/models--([^\s/]+--[^\s/]+)`),
		patternID:  "missing_hf_model",
		category:   "data",
		message:    "Missing HuggingFace model",
		remediable: true,
		extractAssets: func(match []string) []string {
			if len(match) < 2 {
				return nil
			}
			// Convert "meta-llama--Llama-3-8B" to "hf:meta-llama/Llama-3-8B"
			modelID := strings.Replace(match[1], "--", "/", 1)
			return []string{"hf:" + modelID}
		},
	},
	{
		re:         regexp.MustCompile(`(?i)(?:FileNotFoundError|OSError|No such file or directory).*huggingface/hub/datasets--([^\s/]+--[^\s/]+)`),
		patternID:  "missing_hf_dataset",
		category:   "data",
		message:    "Missing HuggingFace dataset",
		remediable: true,
		extractAssets: func(match []string) []string {
			if len(match) < 2 {
				return nil
			}
			datasetID := strings.Replace(match[1], "--", "/", 1)
			return []string{"hf:" + datasetID}
		},
	},
	{
		re:         regexp.MustCompile(`(?i)(?:FileNotFoundError|No such file or directory): '([^']+)'`),
		patternID:  "missing_file",
		category:   "data",
		message:    "Missing file in working directory",
		remediable: true,
		extractAssets: func(match []string) []string {
			// Don't extract asset refs for generic missing files
			return nil
		},
	},
}

// Code patterns: Python errors that indicate code bugs (not auto-remediable by default)
var codePatterns = []*pattern{
	{
		re:        regexp.MustCompile(`ModuleNotFoundError: No module named '([^']+)'`),
		patternID: "missing_import",
		category:  "code",
		message:   "Missing Python module",
	},
	{
		re:        regexp.MustCompile(`ImportError: cannot import name '([^']+)'`),
		patternID: "import_error",
		category:  "code",
		message:   "Python import error",
	},
	{
		re:        regexp.MustCompile(`AttributeError: .+ has no attribute '([^']+)'`),
		patternID: "attribute_error",
		category:  "code",
		message:   "Python attribute error",
	},
	{
		re:        regexp.MustCompile(`NameError: name '([^']+)' is not defined`),
		patternID: "name_error",
		category:  "code",
		message:   "Python name error",
	},
	{
		re:        regexp.MustCompile(`SyntaxError: (.+)`),
		patternID: "syntax_error",
		category:  "code",
		message:   "Python syntax error",
	},
}

// Environment patterns: resource/runtime errors (not remediable)
var envPatterns = []*pattern{
	{
		re:        regexp.MustCompile(`(?:CUDA out of memory|torch\.cuda\.OutOfMemoryError)`),
		patternID: "gpu_oom",
		category:  "environment",
		message:   "GPU out of memory",
	},
	{
		re:             regexp.MustCompile(`RuntimeError: CUDA error`),
		patternID:      "cuda_error",
		category:       "environment",
		message:        "CUDA runtime error",
		fatalAtRuntime: true,
	},
	{
		re:             regexp.MustCompile(`(?:CUDA unknown error|CUDA error: unknown error|cannot re-initialize CUDA|all CUDA-capable devices are busy or unavailable)`),
		patternID:      "cuda_fatal",
		category:       "environment",
		message:        "CUDA device unrecoverable — GPU state corrupted or device lost",
		fatalAtRuntime: true,
	},
	{
		re:        regexp.MustCompile(`(?:ENOSPC|No space left on device)`),
		patternID: "disk_full",
		category:  "environment",
		message:   "Disk full — if HF models were downloaded at runtime, declare them with --input hf:<model-id> so the disk estimator accounts for their size",
	},
}

// CheckFatalAtRuntime scans log content for patterns that indicate the job
// should be killed immediately (e.g., unrecoverable CUDA errors that make
// the GPU unusable). Returns the first matching diagnosis, or nil.
func CheckFatalAtRuntime(logContent string) *ErrorDiagnosis {
	for _, p := range envPatterns {
		if p.fatalAtRuntime {
			if d := p.Match(logContent); d != nil {
				return d
			}
		}
	}
	return nil
}
