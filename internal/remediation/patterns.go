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

type failurePatternRule struct {
	patternID  string
	category   string
	message    string
	confidence float64
	re         *regexp.Regexp
	details    func([]string, string) map[string]any
}

var failurePatternRules = []failurePatternRule{
	{
		patternID:  "preempted",
		category:   "environment",
		message:    "Cloud instance was preempted",
		confidence: 0.95,
		re:         regexp.MustCompile(`(?i)(preempt(?:ed|ion)|spot interruption|instance reclaimed|cloud_outcome[=:]\s*preempted)`),
		details: func(match []string, logContent string) map[string]any {
			details := map[string]any{}
			if provider := firstSubmatch(`(?i)\b(vastai|runpod|aws|gcp|azure|fly)\b`, logContent); provider != "" {
				details["provider"] = strings.ToLower(provider)
			}
			if notice := firstIntSubmatch(`(?i)(\d+)\s*(?:seconds|secs|s).*?(?:preempt|terminat|interrupt)`, logContent); notice > 0 {
				details["notice_seconds_before_termination"] = notice
			}
			return details
		},
	},
	{
		patternID:  "gpu_oom",
		category:   "environment",
		message:    "GPU out of memory",
		confidence: 0.95,
		re:         regexp.MustCompile(`(?is)(?:CUDA|HIP).*out of memory|torch\.cuda\.OutOfMemoryError`),
		details: func(match []string, logContent string) map[string]any {
			details := map[string]any{}
			if requested := parseMemoryMiB(firstSubmatch(`(?i)Tried to allocate\s+([0-9.]+\s*(?:GiB|MiB|GB|MB))`, logContent)); requested > 0 {
				details["requested_mib"] = requested
			}
			if deviceID := firstIntSubmatch(`(?i)(?:GPU|CUDA device)\s+(\d+)`, logContent); deviceID >= 0 {
				details["cuda_device_id"] = deviceID
			}
			return details
		},
	},
	{
		patternID:  "cuda_error",
		category:   "environment",
		message:    "CUDA runtime error",
		confidence: 0.9,
		re:         regexp.MustCompile(`(?is)CUDA error|cuFFT error|cuBLAS error|CUDNN_STATUS_|CUDA_ERROR_[A-Z0-9_]+|illegal memory access`),
		details: func(match []string, logContent string) map[string]any {
			details := map[string]any{}
			if code := firstIntSubmatch(`(?i)CUDA(?: error)?:\s*(\d+)`, logContent); code >= 0 {
				details["cuda_error_code"] = code
			}
			if errStr := firstSubmatch(`(?i)(CUDA_ERROR_[A-Z0-9_]+|CUDNN_STATUS_[A-Z0-9_]+|cuBLAS error[^.\n]*|cuFFT error[^.\n]*)`, logContent); errStr != "" {
				details["cuda_error_str"] = strings.TrimSpace(errStr)
			}
			if kernel := firstSubmatch(`(?i)kernel(?: name)?[:=]\s*([A-Za-z0-9_.$-]+)`, logContent); kernel != "" {
				details["kernel_name"] = kernel
			}
			return details
		},
	},
	{
		patternID:  "disk_full",
		category:   "environment",
		message:    "Disk full or quota exceeded",
		confidence: 0.95,
		re:         regexp.MustCompile(`(?is)No space left on device|disk quota exceeded|EDQUOT|ENOSPC`),
	},
	{
		patternID:  "ssh_disconnect",
		category:   "environment",
		message:    "SSH connection lost",
		confidence: 0.85,
		re:         regexp.MustCompile(`(?is)Connection reset|Broken pipe|Host is unreachable|No route to host|ssh:.*disconnect`),
	},
	{
		patternID:  "timeout",
		category:   "environment",
		message:    "Execution timed out",
		confidence: 0.9,
		re:         regexp.MustCompile(`(?is)timed out|timeout|deadline exceeded|exceeded .*max(?:imum)? time|SIGTERM.*budget`),
		details: func(match []string, logContent string) map[string]any {
			details := map[string]any{}
			if budget := firstIntSubmatch(`(?i)(?:budget|max(?:imum)? time|timeout)\D+(\d+)\s*(?:seconds|secs|s)\b`, logContent); budget > 0 {
				details["budget_seconds"] = budget
			}
			if elapsed := firstIntSubmatch(`(?i)elapsed\D+(\d+)\s*(?:seconds|secs|s)\b`, logContent); elapsed > 0 {
				details["elapsed_seconds"] = elapsed
			}
			switch {
			case strings.Contains(strings.ToLower(logContent), "provider"):
				details["enforcer"] = "provider"
			case strings.Contains(strings.ToLower(logContent), "wrapper"):
				details["enforcer"] = "wrapper"
			default:
				details["enforcer"] = "agent"
			}
			return details
		},
	},
	{
		patternID:  "module_not_found",
		category:   "code",
		message:    "Missing Python module or import",
		confidence: 0.9,
		re:         regexp.MustCompile(`(?is)ModuleNotFoundError:\s*No module named ['"]([^'"]+)['"]|ImportError:|cannot import name|(?:error while loading shared libraries|cannot open shared object file)`),
		details: func(match []string, logContent string) map[string]any {
			details := map[string]any{}
			if len(match) > 1 && strings.TrimSpace(match[1]) != "" {
				details["missing_module"] = strings.TrimSpace(match[1])
			} else if name := firstSubmatch(`(?i)cannot import name ['"]([^'"]+)['"]`, logContent); name != "" {
				details["missing_module"] = name
			}
			if strings.Contains(strings.ToLower(logContent), "pip install") || strings.Contains(strings.ToLower(logContent), "uv add") {
				details["installed_packages"] = "install hint present in log"
			}
			return details
		},
	},
	{
		patternID:  "assert_failure",
		category:   "code",
		message:    "Assertion or runtime failure",
		confidence: 0.8,
		re:         regexp.MustCompile(`(?is)Traceback \(most recent call last\):.*(?:AssertionError|RuntimeError)`),
	},
	{
		patternID:  "subprocess_failure",
		category:   "code",
		message:    "Subprocess exited non-zero",
		confidence: 0.75,
		re:         regexp.MustCompile(`(?is)subprocess\.(?:CalledProcessError|run|check_call|check_output)|Command .* returned non-zero exit status|returned non-zero exit status \d+`),
	},
}

func matchFailurePattern(logContent, detectedBy string) *ErrorDiagnosis {
	for _, rule := range failurePatternRules {
		match := rule.re.FindStringSubmatch(logContent)
		if match == nil {
			continue
		}
		d := &ErrorDiagnosis{
			Pattern:    rule.patternID,
			Category:   rule.category,
			Message:    rule.message,
			Remediable: false,
			Details:    match[0],
		}
		if rule.details != nil {
			d.StructuredDetails = rule.details(match, logContent)
		}
		if rule.patternID == "gpu_oom" {
			enrichGPUOOMDiagnosis(logContent, d)
		}
		stampDiagnosis(d, logContent, detectedBy, rule.confidence)
		return d
	}
	tail := evidenceTail(logContent)
	d := &ErrorDiagnosis{
		Pattern:  "unknown",
		Category: "unknown",
		Message:  "Unknown failure",
		Details:  tail,
	}
	stampDiagnosis(d, logContent, detectedBy, 0.1)
	return d
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

func evidenceTail(logContent string) string {
	const maxChars = 800
	logContent = strings.TrimSpace(logContent)
	if len(logContent) <= maxChars {
		return logContent
	}
	return logContent[len(logContent)-maxChars:]
}

func firstSubmatch(pattern, text string) string {
	re := regexp.MustCompile(pattern)
	match := re.FindStringSubmatch(text)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func firstIntSubmatch(pattern, text string) int {
	value := firstSubmatch(pattern, text)
	if value == "" {
		return -1
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return n
}

func parseMemoryMiB(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	fields := strings.Fields(value)
	if len(fields) != 2 {
		return 0
	}
	amount, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	switch strings.ToLower(fields[1]) {
	case "gib", "gb":
		return int(math.Ceil(amount * 1024))
	case "mib", "mb":
		return int(math.Ceil(amount))
	default:
		return 0
	}
}
