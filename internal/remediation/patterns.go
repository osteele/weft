package remediation

import (
	"math"
	"regexp"
	"sort"
	"strconv"
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
	// enrich can add structured fields to the diagnosis using full log content.
	enrich func(logContent string, diagnosis *ErrorDiagnosis)
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
	if p.enrich != nil {
		p.enrich(logContent, d)
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
		enrich:    enrichGPUOOMDiagnosis,
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

var gpuOOMProcessPattern = regexp.MustCompile(`Process\s+(\d+)\s+has\s+([0-9]+(?:\.[0-9]+)?)\s+GiB in use`)

func enrichGPUOOMDiagnosis(logContent string, diagnosis *ErrorDiagnosis) {
	if diagnosis == nil {
		return
	}

	matches := gpuOOMProcessPattern.FindAllStringSubmatch(logContent, -1)
	if len(matches) == 0 {
		return
	}

	processes := make([]GPUOOMProcess, 0, len(matches))
	for _, m := range matches {
		if len(m) != 3 {
			continue
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		memGiB, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			continue
		}
		processes = append(processes, GPUOOMProcess{
			PID:       pid,
			MemoryGiB: memGiB,
		})
	}
	if len(processes) == 0 {
		return
	}

	sort.Slice(processes, func(i, j int) bool {
		if processes[i].MemoryGiB == processes[j].MemoryGiB {
			return processes[i].PID < processes[j].PID
		}
		return processes[i].MemoryGiB > processes[j].MemoryGiB
	})

	diagnosis.GPUOOMProcesses = processes
	diagnosis.GPUOOMMainPID = processes[0].PID

	if len(processes) > 1 {
		extra := processes[1]
		diagnosis.GPUOOMExtraPID = extra.PID
		diagnosis.GPUOOMExtraGiB = extra.MemoryGiB
		// Suggest enough headroom for the additional process plus a small buffer.
		diagnosis.GPUOOMHintDeltaGB = int(math.Ceil(extra.MemoryGiB)) + 1
		diagnosis.GPUOOMNotes = "PIDs are container-local; identical PID values can appear across different containers."
	}
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
