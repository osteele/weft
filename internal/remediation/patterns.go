package remediation

import (
	"fmt"
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
	solution       string
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
	solution   string
	confidence float64
	re         *regexp.Regexp
	match      func(string) []string
	details    func([]string, string) map[string]any
}

// failurePatternRules is the ordered post-mortem classifier table consumed by
// matchFailurePattern. Matching is first-match-wins, so precedence is the
// concatenation order of the tiers below (see specs/diagnosis.allium).
// Add new rules to the tier matching their specificity instead of appending
// here; TestFailurePatternTiers pins the tier ordering and the
// specific-before-generic contract.
var failurePatternRules = concatRuleTiers(failurePatternTiers)

// ruleTier names one precedence band of the assembled failurePatternRules.
type ruleTier struct {
	name  string
	rules []failurePatternRule
}

// failurePatternTiers assembles the classifier in decreasing specificity.
var failurePatternTiers = []ruleTier{
	{name: "specific-cause", rules: specificCauseRules},
	{name: "framework-setup", rules: frameworkSetupRules},
	{name: "environment-signal", rules: environmentSignalRules},
	{name: "runtime-library-mismatch", rules: runtimeLibraryMismatchRules},
	{name: "generic", rules: genericRules},
}

func concatRuleTiers(tiers []ruleTier) []failurePatternRule {
	var out []failurePatternRule
	for _, t := range tiers {
		out = append(out, t.rules...)
	}
	return out
}

// specificCauseRules diagnose a concrete root cause from a narrow signature
// (hardware fault, disk full, version/driver mismatch, first-party sys.path
// miss). They must precede the framework-setup and generic tiers: several of
// the failures below also surface generic ImportError/timeout/subprocess
// text that the later tiers would mislabel.
var specificCauseRules = []failurePatternRule{
	{
		patternID:  "cuda_hardware_fault",
		category:   "environment",
		message:    "CUDA hardware or interconnect fault",
		solution:   "Retry on a fresh instance; the failing rental likely has a bad GPU, NVLink/peer-memory path, or driver/device state.",
		confidence: 0.95,
		re:         regexp.MustCompile(`(?is)(?:peer GPU memory|NVLink|uncorrectable ECC|Xid)`),
	},
	{
		patternID:  "cli_argument_drift",
		category:   "environment",
		message:    "CLI argument is not supported by the resolved tool version",
		solution:   "The resolved tool version dropped or renamed the reported flag; pin the tool version that supports it or update the flag.",
		confidence: 0.9,
		re:         regexp.MustCompile(`(?is)(?:error:\s*)?unrecognized arguments?:\s+(--[A-Za-z0-9][A-Za-z0-9_-]*)`),
		details: func(match []string, logContent string) map[string]any {
			details := map[string]any{}
			if len(match) > 1 {
				details["flag"] = strings.TrimSpace(match[1])
			}
			if tool := firstSubmatch(`(?m)^(?:usage:\s*)?([A-Za-z0-9_.-]+)(?:\s|\[)`, logContent); tool != "" {
				details["tool"] = tool
			}
			return details
		},
	},
	{
		patternID:  "python_attribute_version_mismatch",
		category:   "environment",
		message:    "Python library version mismatch during model or tokenizer load",
		solution:   "Pin a compatible pair of the model-serving library and transformers/tokenizers, or update the code for the resolved APIs.",
		confidence: 0.85,
		re:         regexp.MustCompile(`(?is)(?:AutoTokenizer|AutoModel|transformers|tokenizer|model|vllm).{0,160}AttributeError:.*has no attribute|AttributeError:.*has no attribute.*(?:AutoTokenizer|AutoModel|transformers|tokenizer|model|vllm)`),
	},
	{
		patternID:  "disk_full",
		category:   "environment",
		message:    "Disk full or quota exceeded",
		solution:   "Retry with a larger --disk or --runtime-disk budget, reduce declared inputs/cache use, or use a rental with more free disk.",
		confidence: 0.95,
		re:         regexp.MustCompile(`(?is)No space left on device|disk quota exceeded|EDQUOT|ENOSPC`),
	},
	{
		patternID:  "cuda_image_wheel_mismatch",
		category:   "environment",
		message:    "Selected CUDA image is older than the resolved Python CUDA wheels",
		solution:   "Use an image whose CUDA version is at least the wheel CUDA requirement, pin older compatible wheels, or omit the image and let weft choose one.",
		confidence: 0.9,
		re:         regexp.MustCompile(`(?is)(?:engine core|engine_core|vllm).*?(?:CUDA|cu12[0-9]|driver|wheel).*?(?:mismatch|incompatible|too old|failed)|(?:CUDA|driver).*?(?:too old|insufficient|incompatible).*?(?:vllm|engine core|engine_core|wheel)`),
	},
	{
		patternID:  "cuda_driver_too_old",
		category:   "environment",
		message:    "NVIDIA driver is too old for the selected CUDA runtime",
		solution:   "Retry with placement restricted to a provider/host that reports compatible CUDA support, or use a PyTorch/runtime image built for the host's older CUDA driver.",
		confidence: 0.98,
		re:         regexp.MustCompile(`(?is)NVIDIA driver on your system is too old\s*\(found version\s+([0-9]+)\)|driver version is insufficient for CUDA runtime version`),
		details: func(match []string, logContent string) map[string]any {
			details := map[string]any{}
			found := ""
			if len(match) > 1 {
				found = strings.TrimSpace(match[1])
			}
			if found == "" {
				found = firstSubmatch(`(?i)found version\s+([0-9]+)`, logContent)
			}
			if found != "" {
				details["found_driver_api_version"] = found
				if compat := cudaCompatFromDriverAPIVersion(found); compat != "" {
					details["found_cuda_compatibility"] = compat
				}
			}
			return details
		},
	},
	{
		patternID:  "tempdir_unusable",
		category:   "environment",
		message:    "No writable temporary directory",
		solution:   "Run with TMPDIR set to a writable project path such as $PWD/output/tmp. Current weft agents create this automatically for new runs unless TMPDIR is already set.",
		confidence: 0.95,
		re:         regexp.MustCompile(`(?is)No usable temporary directory found|could not find a usable temporary directory`),
	},
	{
		patternID:  "python_first_party_import_path",
		category:   "code",
		message:    "First-party Python package is not on sys.path",
		solution:   "Run the script through uv without an extra python interpreter layer, for example `uv run experiments/script.py` instead of `uv run python experiments/script.py`.",
		confidence: 0.95,
		match:      firstPartyImportPathMatch,
		details: func(match []string, logContent string) map[string]any {
			details := map[string]any{}
			if len(match) > 1 {
				details["missing_module"] = match[1]
			}
			if len(match) > 2 {
				details["script"] = match[2]
			}
			return details
		},
	},
}

// frameworkSetupRules diagnose incomplete serving-framework runtimes
// (missing framework module or shared library). They must precede the
// generic module_not_found rule, and the first-party import rule in
// specificCauseRules must precede them (wb27): a first-party ImportError
// traceback can otherwise satisfy these regexes' generic ImportError arms.
var frameworkSetupRules = []failurePatternRule{
	{
		patternID:  "sglang_setup",
		category:   "environment",
		message:    "SGLang runtime setup is incomplete",
		solution:   "Use the documented SGLang path: a script PEP 723 block or .weft.toml image override with lmsysorg/sglang:v0.5.10.post1 plus the required CUDA/driver floors.",
		confidence: 0.9,
		re:         regexp.MustCompile(`(?im)(ModuleNotFoundError:\s*No module named ['"](sglang|sgl[-_]?kernel|flashinfer|outlines-core|libnuma)(?:[.'"][^'"]*)?['"]|ImportError:[^\n]*(sglang|sgl[-_]?kernel|flashinfer|outlines-core|libnuma)|cannot import name[^\n]*(sglang|sgl[-_]?kernel|flashinfer|outlines-core|libnuma)|(?:error while loading shared libraries|cannot open shared object file)[^\n]*(sglang|sgl[-_]?kernel|flashinfer|outlines-core|libnuma))`),
		details: func(match []string, logContent string) map[string]any {
			return map[string]any{"framework": "sglang"}
		},
	},
	{
		patternID:  "vllm_setup",
		category:   "environment",
		message:    "vLLM runtime setup is incomplete",
		solution:   "Use the documented vLLM path: declare the vLLM dependency in PEP 723 or pyproject.toml, run through uv, and let weft select a PyTorch CUDA image and disk headroom.",
		confidence: 0.9,
		re:         regexp.MustCompile(`(?im)(ModuleNotFoundError:\s*No module named ['"](vllm|flashinfer)(?:[.'"][^'"]*)?['"]|ImportError:[^\n]*(vllm|flashinfer)|cannot import name[^\n]*(vllm|flashinfer)|(?:error while loading shared libraries|cannot open shared object file)[^\n]*(vllm|flashinfer))`),
		details: func(match []string, logContent string) map[string]any {
			return map[string]any{"framework": "vllm"}
		},
	},
}

// environmentSignalRules classify broad runtime/environment signals
// (preemption, OOM, CUDA runtime errors, connectivity, timeouts). Their
// regexes are loose, so any narrower cause must be diagnosed by an earlier
// tier before these get a chance to match.
var environmentSignalRules = []failurePatternRule{
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
		// gpu_oom is also defined in envPatterns. Same pattern ID and
		// semantics, different matcher breadth and consumer: this entry is
		// the post-mortem classifier (broader regex, also matches HIP OOM);
		// the envPatterns entry serves the live-log runtime table. Both are
		// enriched by enrichGPUOOMDiagnosis.
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
		patternID:  "ssh_disconnect",
		category:   "environment",
		message:    "SSH connection lost",
		confidence: 0.85,
		re:         regexp.MustCompile(`(?is)ssh:.*(?:Connection reset|disconnect|Broken pipe)|Connection to .* closed by remote host|Host is unreachable|No route to host`),
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
}

// runtimeLibraryMismatchRules diagnose failures that surface as Python
// import/loader errors but whose cause is an incoherent userland or CUDA
// library stack. They must precede the generic module_not_found rule so the
// mismatch is named instead of "missing module".
var runtimeLibraryMismatchRules = []failurePatternRule{
	{
		patternID:  "glibcxx_version_not_found",
		category:   "environment",
		message:    "Host libstdc++ is too old for the job runtime",
		solution:   "Retry on a host or image with a newer OS userland, such as Ubuntu 22.04 or newer, or install a compatible libstdc++ for the job environment.",
		confidence: 0.98,
		re:         regexp.MustCompile("(?is)version [`'\"]GLIBCXX_([0-9.]+)['\"] not found"),
		details: func(match []string, logContent string) map[string]any {
			details := map[string]any{}
			if len(match) > 1 {
				details["required_glibcxx"] = strings.TrimSpace(match[1])
			}
			if soFile := firstSubmatch(`(?i)(/[^\s:]+\.so(?:\.\d+)*):[^\n]*version [`+"`"+`'"]GLIBCXX_`, logContent); soFile != "" {
				details["loaded_library"] = soFile
			}
			return details
		},
	},
	{
		patternID:  "glibc_version_not_found",
		category:   "environment",
		message:    "Host glibc is too old for the job runtime",
		solution:   "Retry on a host or image with a newer OS userland, or use binaries built for the host's older glibc.",
		confidence: 0.98,
		re:         regexp.MustCompile("(?is)version [`'\"]GLIBC_([0-9.]+)['\"] not found"),
		details: func(match []string, logContent string) map[string]any {
			details := map[string]any{}
			if len(match) > 1 {
				details["required_glibc"] = strings.TrimSpace(match[1])
			}
			if soFile := firstSubmatch(`(?i)(/[^\s:]+\.so(?:\.\d+)*):[^\n]*version [`+"`"+`'"]GLIBC_`, logContent); soFile != "" {
				details["loaded_library"] = soFile
			}
			return details
		},
	},
	{
		// CUDA / NVIDIA shared-library symbol mismatch. Fires before the
		// generic module_not_found rule below: the failure surfaces as a
		// Python ImportError, but the cause is an incoherent CUDA stack
		// (e.g. image-provided torch + lockfile-installed nvidia-* wheels
		// disagreeing on the CUDA toolkit version), not a missing module.
		patternID:  "cuda_lib_symbol_mismatch",
		category:   "environment",
		message:    "CUDA/NVIDIA shared library symbol mismatch",
		solution:   "The Python torch stack and its bundled NVIDIA libraries (cusparse, cublas, cudnn, nvJitLink, etc.) are from incompatible CUDA toolkit versions. This typically happens when an image-provided torch coexists with lockfile-installed nvidia-* wheels at a different CUDA version. Fix by either (a) pinning a Docker image whose CUDA matches the project's torch pin, or (b) letting uv reinstall torch from the lockfile so all CUDA libs come from one coherent source.",
		confidence: 0.95,
		re:         regexp.MustCompile(`(?is)ImportError:[^\n]*\.so(?:\.\d+)*[^\n]*undefined symbol:[^\n]*(?:__nv|libnv|libcu|cublas|cusparse|cudnn|cufft|curand|cusolver|nccl|nvJitLink)`),
		details: func(match []string, logContent string) map[string]any {
			details := map[string]any{}
			if sym := firstSubmatch(`(?i)undefined symbol:\s*([A-Za-z0-9_]+)`, logContent); sym != "" {
				details["undefined_symbol"] = sym
			}
			if lib := firstSubmatch(`(?i)version\s+(lib[A-Za-z0-9_.+-]+\.so(?:\.\d+)*)`, logContent); lib != "" {
				details["expected_in_library"] = lib
			}
			if soFile := firstSubmatch(`(?i)(/[^\s:]+\.so(?:\.\d+)*):\s*undefined symbol`, logContent); soFile != "" {
				details["loaded_library"] = soFile
			}
			return details
		},
	},
}

// genericRules are catch-alls that match wide classes of Python failures.
// They must come last: every specific-cause, framework-setup, and
// runtime-library-mismatch diagnosis would also satisfy one of these.
var genericRules = []failurePatternRule{
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

func firstPartyImportPathMatch(logContent string) []string {
	missing := firstSubmatch(`(?m)ModuleNotFoundError:\s*No module named ['"]([A-Za-z_][A-Za-z0-9_]*)['"]`, logContent)
	if missing == "" {
		return nil
	}
	command := firstSubmatch(`(?m)^cmd:\s*(.+)$`, logContent)
	if command == "" {
		return nil
	}
	script := pythonScriptArg(command)
	if script == "" {
		return nil
	}
	topPackage, _, ok := strings.Cut(script, "/")
	if !ok || topPackage != missing {
		return nil
	}
	tracebackLine := firstSubmatch(`(?m)^(\s*from\s+`+regexp.QuoteMeta(missing)+`\.[A-Za-z0-9_.]+\s+import\s+.+)$`, logContent)
	if tracebackLine == "" {
		return nil
	}
	return []string{tracebackLine + "\n" + "ModuleNotFoundError: No module named '" + missing + "'", missing, script}
}

func pythonScriptArg(command string) string {
	fields := strings.Fields(command)
	for i, field := range fields {
		if !pythonExecutableToken(field) || i+1 >= len(fields) {
			continue
		}
		next := strings.Trim(fields[i+1], `"'`)
		if strings.HasSuffix(next, ".py") && strings.Contains(next, "/") {
			return next
		}
	}
	return ""
}

func pythonExecutableToken(token string) bool {
	token = strings.Trim(token, `"'`)
	if token == "python" {
		return true
	}
	if strings.HasPrefix(token, "python") {
		rest := strings.TrimPrefix(token, "python")
		return regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`).MatchString(rest)
	}
	return false
}

func cudaCompatFromDriverAPIVersion(version string) string {
	if len(version) < 4 {
		return ""
	}
	n, err := strconv.Atoi(version)
	if err != nil {
		return ""
	}
	major := n / 1000
	minor := (n % 1000) / 10
	if major == 0 {
		return ""
	}
	return fmt.Sprintf("%d.%d", major, minor)
}

func matchFailurePattern(logContent, detectedBy string) *ErrorDiagnosis {
	for _, rule := range failurePatternRules {
		var match []string
		if rule.match != nil {
			match = rule.match(logContent)
		} else {
			match = rule.re.FindStringSubmatch(logContent)
		}
		if match == nil {
			continue
		}
		d := &ErrorDiagnosis{
			Pattern:    rule.patternID,
			Category:   rule.category,
			Message:    rule.message,
			Solution:   rule.solution,
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
		Solution:   p.solution,
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
		re:         regexp.MustCompile(`(?is)uv run python\s+\S+.*(?:FileNotFoundError|No such file or directory):\s*(?:\[[^\]]+\]\s*)?(?:'|")?(vllm|sglang|accelerate|torchrun)(?:'|")?`),
		patternID:  "pep723_console_script_missing",
		category:   "environment",
		message:    "PEP 723 inline dependencies were not installed for the invoked console script",
		solution:   "Invoke the script as `uv run <script>` instead of `uv run python <script>` so uv installs the script's PEP 723 inline dependencies and exposes console scripts.",
		remediable: false,
	},
	{
		re:         regexp.MustCompile(`(?is)(?:RepositoryNotFoundError|Repository Not Found).*?huggingface\.co/(?:api/models/)?((?:[^/\s'")]+/[^/\s'")]+)|[^/\s'")]+)/(?:resolve|tree)\b`),
		patternID:  "hf_model_repo_not_found",
		category:   "data",
		message:    "HuggingFace model repo not found",
		solution:   "Use the full Hugging Face model repo ID in --input, for example `hf:org/model`, then resubmit.",
		remediable: false,
		extractAssets: func(match []string) []string {
			if len(match) < 2 {
				return nil
			}
			return []string{"hf:" + match[1]}
		},
	},
	{
		// HuggingFace gated-repo access denied. The repo exists but the
		// HF_TOKEN doesn't have access approval for it. Distinguished from
		// "missing model" (no cache entry) and from "timeout" (the request
		// completed quickly with an auth response) — both of which the
		// classifier used to mis-attribute this to before this rule existed.
		// Lives in dataPatterns so it wins over the generic `timeout`
		// pattern in failurePatternRules, which would otherwise match a
		// "HTTPSConnectionPool ... read timed out" tail if HF was slow to
		// return the 401, or a "Cannot access gated repo for url" phrase
		// that happens to contain the substring "access ... gated".
		re:         regexp.MustCompile(`(?is)(?:GatedRepoError|access .{0,40}?gated repo|HfHubHTTPError.*?\b401\b.*?(?:gated|Unauthorized)|\b401 Client Error.*?(?:gated|Unauthorized))`),
		patternID:  "hf_gated_repo",
		category:   "data",
		message:    "HuggingFace gated repo: token lacks access approval",
		remediable: false,
		// extractAssets intentionally nil — the asset isn't auto-fetchable;
		// the user has to request access on huggingface.co first.
	},
	{
		re:         regexp.MustCompile(`(?is)huggingface\.co/([^/\s'")]+/[^/\s'")]+)/resolve/.*?(?:LocalEntryNotFoundError|couldn'?t connect to 'https://huggingface\.co'.*?cached files|cannot find the requested files in the local cache)`),
		patternID:  "hf_network_cache_miss",
		category:   "data",
		message:    "HuggingFace model unavailable: network request failed and local cache is missing",
		solution:   "Declare the model as a Weft input (for example `--input hf:<model-id>`) or pre-stage it on the target host, then retry.",
		remediable: false,
		extractAssets: func(match []string) []string {
			if len(match) < 2 {
				return nil
			}
			return []string{"hf:" + match[1]}
		},
	},
	{
		// Transient HuggingFace network failure: a reset/interrupted live
		// request co-occurring with an HF give-up. The asset exists and is
		// reachable on a clean sweep, so this is retry-safe (distinct from
		// hf_network_cache_miss, which is a permanent missing-declaration).
		// Gated on the reset signal — not bare LocalEntryNotFoundError — so a
		// genuinely missing/gated asset does not get retried.
		re:         regexp.MustCompile(`(?is)(?:ConnectionResetError|Connection reset by peer).*?(?:LocalEntryNotFoundError|[Cc]ouldn'?t reach\b.*?\bon the Hub)`),
		patternID:  "hf_transient_network",
		category:   "data",
		message:    "HuggingFace fetch failed on a transient network reset; the asset exists but the live request was interrupted",
		solution:   "Auto-retried on a fresh sweep. Declare the asset as a Weft input (hf:<model> / hf-dataset:<dataset>) so it is pre-staged and the job can run offline.",
		remediable: true,
	},
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
		// Duplicate of the failurePatternRules gpu_oom entry (same pattern
		// ID and semantics). This copy exists for the live-log runtime
		// table; it is intentionally NOT fatalAtRuntime — an OOM'd job dies
		// on its own, so CheckFatalAtRuntime must not kill it early. The
		// post-mortem path classifies gpu_oom via failurePatternRules,
		// whose regex is broader (also matches HIP OOM).
		re:        regexp.MustCompile(`(?:CUDA out of memory|torch\.cuda\.OutOfMemoryError)`),
		patternID: "gpu_oom",
		category:  "environment",
		message:   "GPU out of memory",
		enrich:    enrichGPUOOMDiagnosis,
	},
	{
		re:             regexp.MustCompile(`(?is)(?:peer GPU memory|NVLink|uncorrectable ECC|Xid)`),
		patternID:      "cuda_hardware_fault",
		category:       "environment",
		message:        "CUDA hardware or interconnect fault",
		fatalAtRuntime: true,
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
	{
		re:             regexp.MustCompile(`(?i)NVIDIA driver on your system is too old|driver version is insufficient for CUDA runtime version`),
		patternID:      "cuda_driver_too_old",
		category:       "environment",
		message:        "NVIDIA driver is too old for the selected CUDA runtime",
		fatalAtRuntime: true,
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
