package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/runner"
)

// jobSequenceConfig holds parameters for runJobSequence.
type jobSequenceConfig struct {
	R2Bucket            string
	InstanceID          int64
	PhaseKey            string
	LogDir              string
	DiskPath            string
	MaxTime             time.Duration // 0 = no limit
	StartTime           time.Time     // for time budget accounting
	OnPhase             func(string)  // update current phase string (for heartbeat)
	SkipWorkdirDeletion bool          // disable background workdir cleanup (for debugging)
	GPUWarmup           bool          // run CUDA warmup before first benchmark job
	DisableJobPolling   bool          // disable dynamic mid-job pickup for nested packed-slot workers
	ConcurrentSlot      bool          // this sequence is one worker inside a packed concurrent slot group
	// CostPerHourCents drives hang-watchdog tier selection; 0 means on-prem
	// or unknown, which picks conservative thresholds.
	CostPerHourCents int

	// Provider and InstanceType are exposed to job processes via
	// WEFT_PROVIDER / WEFT_INSTANCE_TYPE env vars. Resumed indicates this
	// container has restarted after a docker stop on the same disk (typical
	// of a pause/resume on interruptible Vast.ai instances).
	Provider     string
	InstanceType string
	Resumed      bool

	// SelfDestructCmd is the provider-specific shell command that terminates
	// this instance from inside the container (e.g. `vastai destroy ...`).
	// Used by infra-failure handlers (setup-timeout prewarm, disk-cap probe,
	// etc.) to terminate the rental and let Weft's reconciler
	// retry the affected jobs on a fresh instance.
	SelfDestructCmd string
}

// agentRentalEnv builds the rental-context env vars set by the agent for
// every job it runs. These let job processes detect that they're on a weft
// rental, identify the provider and rental type, and notice that they've
// been restarted after a container pause (the typical preemption symptom on
// Vast.ai interruptible instances).
func agentRentalEnv(cfg jobSequenceConfig) []string {
	env := []string{
		"WEFT_TARGET_KIND=rental",
		fmt.Sprintf("WEFT_LAUNCH_ID=%d", cfg.InstanceID),
	}
	if cfg.Provider != "" {
		env = append(env, "WEFT_PROVIDER="+cfg.Provider)
	}
	if cfg.InstanceType != "" {
		env = append(env, "WEFT_INSTANCE_TYPE="+cfg.InstanceType)
	}
	if cfg.Resumed {
		env = append(env, "WEFT_RESUMED=1")
	}
	return env
}

func appendAgentJobEnv(env []string, job cloud.AgentJob) []string {
	if job.RestagedOutputs {
		env = append(env, "WEFT_RESTAGED=1")
	}
	return append(env, job.Env...)
}

// outputWindowStartForJob returns the attempt-start bound for the job's
// convention-output uploads, or 0 when the job runs on restaged outputs —
// restaged files carry prior-attempt mtimes and must remain uploadable
// under this attempt (see uploadOutputDirs).
func outputWindowStartForJob(job cloud.AgentJob, startUnix int64) int64 {
	if job.RestagedOutputs {
		return 0
	}
	return startUnix
}

func hasDeclaredHFInput(inputs []string) bool {
	for _, input := range inputs {
		if strings.HasPrefix(input, "hf:") || strings.HasPrefix(input, "hf-dataset:") {
			return true
		}
	}
	return false
}

func hfProvisioningEnv(inputs []string) []string {
	if !hasDeclaredHFInput(inputs) {
		return nil
	}
	return []string{
		"HF_HUB_OFFLINE=0",
		"TRANSFORMERS_OFFLINE=0",
		"HF_DATASETS_OFFLINE=0",
	}
}

func hfDownloadPrewarmEnv(baseEnv []string, inputs []string) []string {
	if !hasDeclaredHFInput(inputs) {
		return baseEnv
	}
	env := append([]string(nil), baseEnv...)
	env = append(env, hfProvisioningEnv(inputs)...)
	return env
}

// pickWatchdogTimeouts selects GPU-idle and stdout-silence timeouts based on
// the cost-per-hour of the current host. Expensive cloud hosts (>= $2/hr) get
// aggressive thresholds so hangs are caught before they burn significant money.
// See specs/job-lifecycle.allium rules GPUIdleKillsJob and StdoutSilenceKillsJob.
func pickWatchdogTimeouts(costPerHourCents int) (gpuIdle, stdoutSilence time.Duration) {
	const expensiveCents = 200 // $2.00/hr
	if costPerHourCents >= expensiveCents {
		return 8 * time.Minute, 12 * time.Minute
	}
	return 20 * time.Minute, 30 * time.Minute
}

func pickSetupTimeout(cfg jobSequenceConfig) time.Duration {
	if cfg.Provider != "" || cfg.CostPerHourCents > 0 {
		return 60 * time.Minute
	}
	return inventory.DefaultSetupTimeout
}

type setupPrewarmResult struct {
	ok       bool
	logPath  string
	setupRan bool
	didWork  bool
	err      error
	// failureReason is the single-line, machine-readable reason persisted
	// for this failed prewarm. Empty means use err.Error().
	failureReason string
	// infraFailure marks prewarm work owned by weft (HF input staging) that
	// failed before the user command started and should terminate the rental
	// as retryable infrastructure.
	infraFailure bool
	// exitInfo carries the underlying RunSetupCommand exit info when the
	// prewarm failed. Specifically used downstream to distinguish setup-
	// phase timeouts (exit 124) from generic prewarm failures — timeouts
	// at this boundary almost always indicate infrastructure problems
	// (rental network throughput, provider issues) rather than user code.
	exitInfo runner.ExitInfo
	// logTail captures the trailing lines of the prewarm log for live
	// stderr output to the operator. It is intentionally kept separate
	// from err so the err message stays single-line; failure_reason is a
	// classification column and must not carry a multi-line script dump
	// (corrupts the TUI footer line and the `Reason:` line of weft info).
	logTail string
}

const hfPrewarmMaxAttempts = 3

var (
	runSetupCommand      = runner.RunSetupCommand
	hfPrewarmBackoffFunc = hfPrewarmBackoff
)

type setupPrewarm struct {
	jobID   int64
	runID   int64
	workDir string
	done    chan setupPrewarmResult
}

type setupPrewarmManager struct {
	mu      sync.Mutex
	active  *setupPrewarm
	results map[int64]setupPrewarmResult
}

func newSetupPrewarmManager() *setupPrewarmManager {
	return &setupPrewarmManager{results: map[int64]setupPrewarmResult{}}
}

func setupPrewarmKey(job cloud.AgentJob) int64 {
	if job.RunID > 0 {
		return job.RunID
	}
	return -job.ID
}

func (m *setupPrewarmManager) startNext(currentIndex int, jobs []cloud.AgentJob, cfg jobSequenceConfig, currentWorkDir string) {
	if m == nil || currentIndex < 0 || currentIndex >= len(jobs) {
		return
	}
	current := jobs[currentIndex]
	if db.HasBenchmarkTag(current.Tags) {
		return
	}
	currentDir := runner.ExpandTilde(currentWorkDir)
	for i := currentIndex + 1; i < len(jobs); i++ {
		next := jobs[i]
		nextDir := runner.ExpandTilde(next.Dir)
		if !setupPrewarmEligible(currentDir, next, nextDir) {
			continue
		}
		key := setupPrewarmKey(next)
		m.mu.Lock()
		if m.active != nil || m.results[key].ok || m.results[key].err != nil {
			m.mu.Unlock()
			return
		}
		pw := &setupPrewarm{
			jobID:   next.ID,
			runID:   next.RunID,
			workDir: nextDir,
			done:    make(chan setupPrewarmResult, 1),
		}
		m.active = pw
		m.mu.Unlock()

		oplog.LogJob(oplog.OpPhaseTransition, next.ID, "", oplog.WithDetail("setup_prewarm_start"))
		go m.run(pw, next, cfg)
		return
	}
}

func setupPrewarmEligible(currentDir string, job cloud.AgentJob, nextDir string) bool {
	if nextDir == "" || nextDir == currentDir {
		return false
	}
	if len(job.CloudAfter) > 0 || db.HasBenchmarkTag(job.Tags) {
		return false
	}
	if len(hfInputAssets(job.Inputs)) > 0 {
		return true
	}
	setupCmd := runner.DetectSetupCommand(nextDir)
	if setupCmd == "" {
		return false
	}
	if setupCmd == "uv sync" {
		scriptMeta, _ := dataloc.ScanScriptMeta(nextDir, job.Command)
		if runner.ShouldSkipSetup(setupCmd, scriptMeta) {
			return false
		}
	}
	return true
}

func orderJobsForSetupOverlap(jobs []cloud.AgentJob) []cloud.AgentJob {
	if len(jobs) < 2 {
		return jobs
	}
	edges, indegree := jobDependencyGraph(jobs)
	remaining := make(map[int64]cloud.AgentJob, len(jobs))
	originalIndex := make(map[int64]int, len(jobs))
	for i, job := range jobs {
		remaining[job.ID] = job
		originalIndex[job.ID] = i
	}
	ordered := make([]cloud.AgentJob, 0, len(jobs))
	for len(remaining) > 0 {
		bestID := int64(0)
		bestSet := false
		for id, job := range remaining {
			if indegree[id] != 0 {
				continue
			}
			if !bestSet || setupOrderLess(job, remaining[bestID], originalIndex) {
				bestID = id
				bestSet = true
			}
		}
		if !bestSet {
			appendRemainingInOriginalOrder(&ordered, jobs, remaining)
			return ordered
		}
		job := remaining[bestID]
		delete(remaining, bestID)
		ordered = append(ordered, job)
		for _, dependentID := range edges[bestID] {
			indegree[dependentID]--
		}
	}
	return ordered
}

func jobDependencyGraph(jobs []cloud.AgentJob) (map[int64][]int64, map[int64]int) {
	ids := make(map[int64]bool, len(jobs))
	edges := make(map[int64][]int64, len(jobs))
	indegree := make(map[int64]int, len(jobs))
	for _, job := range jobs {
		ids[job.ID] = true
		indegree[job.ID] = 0
	}
	addEdge := func(producerID, consumerID int64) {
		if producerID == consumerID || !ids[producerID] || !ids[consumerID] {
			return
		}
		for _, existing := range edges[producerID] {
			if existing == consumerID {
				return
			}
		}
		edges[producerID] = append(edges[producerID], consumerID)
		indegree[consumerID]++
	}
	for _, job := range jobs {
		for _, ref := range job.CloudAfter {
			addEdge(ref.JobID, job.ID)
		}
		for _, spec := range job.Needs {
			parsed, err := runner.ParseNeedsSpec(spec)
			if err == nil && !parsed.IsAsset() {
				addEdge(parsed.Version, job.ID)
			}
		}
	}
	return edges, indegree
}

func setupOrderLess(a, b cloud.AgentJob, originalIndex map[int64]int) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	if aw, bw := setupWeight(a), setupWeight(b); aw != bw {
		return aw < bw
	}
	return originalIndex[a.ID] < originalIndex[b.ID]
}

func appendRemainingInOriginalOrder(ordered *[]cloud.AgentJob, original []cloud.AgentJob, remaining map[int64]cloud.AgentJob) {
	for _, job := range original {
		if _, ok := remaining[job.ID]; ok {
			*ordered = append(*ordered, job)
			delete(remaining, job.ID)
		}
	}
}

func setupWeight(job cloud.AgentJob) int {
	if len(hfInputAssets(job.Inputs)) > 0 {
		return 1
	}
	workDir := runner.ExpandTilde(job.Dir)
	if workDir == "" {
		return 0
	}
	setupCmd := runner.DetectSetupCommand(workDir)
	if setupCmd == "" {
		return 0
	}
	if setupCmd == "uv sync" {
		scriptMeta, _ := dataloc.ScanScriptMeta(workDir, job.Command)
		if runner.ShouldSkipSetup(setupCmd, scriptMeta) {
			return 0
		}
	}
	return 1
}

func (m *setupPrewarmManager) run(pw *setupPrewarm, job cloud.AgentJob, cfg jobSequenceConfig) {
	result := runSetupPrewarm(job, cfg, pw.workDir, true)
	pw.done <- result
	close(pw.done)

	key := setupPrewarmKey(job)
	m.mu.Lock()
	m.results[key] = result
	if m.active == pw {
		m.active = nil
	}
	m.mu.Unlock()
	if result.err != nil {
		oplog.LogJob(oplog.OpJobFail, job.ID, "", oplog.WithDetailf("setup prewarm failed: %v", result.err))
	} else {
		oplog.LogJob(oplog.OpPhaseTransition, job.ID, "", oplog.WithDetail("setup_prewarm_done"))
	}
}

func runSetupPrewarm(job cloud.AgentJob, cfg jobSequenceConfig, workDir string, includeSetup bool) setupPrewarmResult {
	logDir := filepath.Join(os.TempDir(), "weft-setup-prewarm", fmt.Sprintf("%d-%d", job.ID, job.RunID))
	_ = os.RemoveAll(logDir)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return setupPrewarmResult{err: err}
	}
	paths := runner.NewJobPaths(logDir, job.ID)
	if err := os.WriteFile(paths.Log, []byte(fmt.Sprintf("=== PREWARM %s ===\njob_id: %d\ncd: %s\n===\n", time.Now().Format(time.UnixDate), job.ID, workDir)), 0o644); err != nil {
		return setupPrewarmResult{err: err}
	}

	env := agentRentalEnv(cfg)
	if dotenvVars, err := runner.LoadDotenvFiles(workDir); err == nil {
		env = append(env, dotenvVars...)
	}
	env = appendAgentJobEnv(env, job)

	didWork := false
	explicitAssets, bestEffortAssets := splitHFInputAssets(job.Inputs, job.BestEffortInputs)
	if len(explicitAssets) > 0 {
		didWork = true
		if result := runHFDownloadPrewarm(job, cfg, workDir, env, explicitAssets, paths); !result.ok {
			return result
		}
	}
	if len(bestEffortAssets) > 0 {
		didWork = true
		if result := runHFDownloadPrewarm(job, cfg, workDir, env, bestEffortAssets, paths); !result.ok {
			appendBestEffortHFPrewarmWarning(paths.Log, bestEffortAssets, result.err)
		}
	}

	if !includeSetup {
		return setupPrewarmResult{ok: true, didWork: didWork, logPath: paths.Log}
	}

	setupCmd := runner.DetectSetupCommand(workDir)
	if setupCmd == "" {
		return setupPrewarmResult{ok: true, didWork: didWork, logPath: paths.Log}
	}
	if setupCmd == "uv sync" {
		scriptMeta, _ := dataloc.ScanScriptMeta(workDir, job.Command)
		if runner.ShouldSkipSetup(setupCmd, scriptMeta) {
			return setupPrewarmResult{ok: true, didWork: didWork, logPath: paths.Log}
		}
	}
	didWork = true
	ei, err := runSetupCommand(setupCmd, job.ID, workDir, env, paths, pickSetupTimeout(cfg))
	if err != nil {
		return setupPrewarmResult{
			didWork:  true,
			logPath:  paths.Log,
			exitInfo: ei,
			err:      err,
			logTail:  prewarmLogTail(paths.Log),
		}
	}
	if ei.ExitCode != 0 {
		return setupPrewarmResult{
			didWork:  true,
			logPath:  paths.Log,
			exitInfo: ei,
			err:      fmt.Errorf("setup prewarm exit %d", ei.ExitCode),
		}
	}
	return setupPrewarmResult{ok: true, didWork: true, setupRan: true, logPath: paths.Log}
}

func runHFDownloadPrewarm(job cloud.AgentJob, cfg jobSequenceConfig, workDir string, baseEnv []string, assets []dataloc.DataAsset, paths runner.JobPaths) setupPrewarmResult {
	script := hfDownloadScript(assets)
	timeout := pickSetupTimeout(cfg)
	disableXet := false
	var last setupPrewarmResult
	for attempt := 1; attempt <= hfPrewarmMaxAttempts; attempt++ {
		env := hfDownloadPrewarmEnv(baseEnv, job.Inputs)
		if disableXet {
			env = append(env, "HF_HUB_DISABLE_XET=1")
		}
		appendPrewarmStatus(paths.Log, fmt.Sprintf("weft: HF prewarm attempt %d/%d%s\n",
			attempt, hfPrewarmMaxAttempts, hfPrewarmXetSuffix(disableXet)))
		ei, err := runSetupCommand(script, job.ID, workDir, env, paths, timeout)
		if err == nil {
			return setupPrewarmResult{ok: true, didWork: true, logPath: paths.Log}
		}
		failureReason, infra := db.ClassifyInfraFailure(db.PhasePrewarmDownload, ei.ExitCode, "")
		last = setupPrewarmResult{
			didWork:       true,
			logPath:       paths.Log,
			exitInfo:      ei,
			err:           fmt.Errorf("hf prewarm failed exit %d: %w", ei.ExitCode, err),
			failureReason: failureReason,
			infraFailure:  infra,
			logTail:       prewarmLogTail(paths.Log),
		}
		if attempt == hfPrewarmMaxAttempts {
			break
		}
		if hfPrewarmLogHasXetError(paths.Log) && !disableXet {
			disableXet = true
			appendPrewarmStatus(paths.Log, "weft: HF prewarm retry disabling xet after xet transfer error\n")
		}
		time.Sleep(hfPrewarmBackoffFunc(attempt))
	}
	return last
}

func appendBestEffortHFPrewarmWarning(path string, assets []dataloc.DataAsset, err error) {
	for _, asset := range assets {
		appendPrewarmStatus(path, fmt.Sprintf("weft: auto-detected input %s failed to stage; continuing (best-effort)\n", asset.Ref()))
	}
	if err != nil {
		appendPrewarmStatus(path, fmt.Sprintf("weft: best-effort HF prewarm error: %v\n", err))
	}
}

func hfPrewarmXetSuffix(disableXet bool) string {
	if disableXet {
		return " (HF_HUB_DISABLE_XET=1)"
	}
	return ""
}

func hfPrewarmBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 5 * time.Second
	default:
		return 20 * time.Second
	}
}

func appendPrewarmStatus(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

func hfPrewarmLogHasXetError(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return hfPrewarmTextHasXetError(string(data))
}

func hfPrewarmTextHasXetError(text string) bool {
	text = strings.ToLower(text)
	if !strings.Contains(text, "xet") {
		return false
	}
	// Writer/reconstruction failures (xet upload/rebuild path).
	if strings.Contains(text, "file reconstruction error") ||
		strings.Contains(text, "internal writer error") ||
		strings.Contains(text, "background writer channel closed") {
		return true
	}
	// Read/download failures: xet-migrated models whose xet-read-token endpoint
	// 404s HF-side. Retrying with HF_HUB_DISABLE_XET=1 falls back to the classic
	// HTTP transfer, which still works for these repos.
	return strings.Contains(text, "xet-read-token") ||
		strings.Contains(text, "xet_get") ||
		strings.Contains(text, "new_file_download_group")
}

func prewarmLogTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	const max = 2000
	if len(data) > max {
		data = data[len(data)-max:]
		if i := strings.IndexByte(string(data), '\n'); i >= 0 && i+1 < len(data) {
			data = data[i+1:]
		}
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return ""
	}
	return "\nprewarm log tail:\n" + text
}

func hfInputAssets(inputs []string) []dataloc.DataAsset {
	assets := make([]dataloc.DataAsset, 0, len(inputs))
	for _, input := range inputs {
		asset, ok := dataloc.ParseAssetRef(input)
		if !ok {
			continue
		}
		if asset.Kind == dataloc.AssetHFModel || asset.Kind == dataloc.AssetHFDataset {
			assets = append(assets, asset)
		}
	}
	return assets
}

func splitHFInputAssets(inputs, bestEffortInputs []string) (explicit, bestEffort []dataloc.DataAsset) {
	bestEffortSet := make(map[string]bool, len(bestEffortInputs))
	for _, input := range bestEffortInputs {
		bestEffortSet[input] = true
	}
	for _, asset := range hfInputAssets(inputs) {
		if bestEffortSet[asset.Ref()] {
			bestEffort = append(bestEffort, asset)
			continue
		}
		explicit = append(explicit, asset)
	}
	return explicit, bestEffort
}

func hfDownloadScript(assets []dataloc.DataAsset) string {
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	b.WriteString("export PATH=\"$HOME/.local/bin:$HOME/bin:${PATH}\"\n")
	b.WriteString("ensure_hf_download_tool() {\n")
	b.WriteString("  if command -v hf >/dev/null 2>&1 || command -v huggingface-cli >/dev/null 2>&1; then return 0; fi\n")
	b.WriteString("  if command -v uv >/dev/null 2>&1; then uv tool install 'huggingface-hub[hf_xet]' >/dev/null; elif command -v python3 >/dev/null 2>&1; then python3 -m pip install --quiet 'huggingface-hub[hf_xet]'; else echo 'no uv or python3 available to install huggingface-hub' >&2; return 127; fi\n")
	b.WriteString("}\n")
	b.WriteString("hf_download() {\n")
	b.WriteString("  _repo_type=\"$1\"; _repo_id=\"$2\"\n")
	// Skip native-checkpoint dirs (e.g. Meta llama original/consolidated.*.pth)
	// for model repos; transformers/vLLM never load them.
	b.WriteString("  set -- --repo-type \"$_repo_type\"\n")
	b.WriteString("  if [ \"$_repo_type\" = model ]; then set -- \"$@\" --exclude 'original/*'; fi\n")
	b.WriteString("  if command -v hf >/dev/null 2>&1; then hf download \"$@\" \"$_repo_id\" 2>&1; elif command -v huggingface-cli >/dev/null 2>&1; then huggingface-cli download \"$@\" \"$_repo_id\" 2>&1; elif [ \"$_repo_type\" = model ]; then python3 -c 'import sys; from huggingface_hub import snapshot_download; snapshot_download(repo_id=sys.argv[1], repo_type=sys.argv[2], ignore_patterns=[\"original/*\"])' \"$_repo_id\" \"$_repo_type\"; else python3 -c 'import sys; from huggingface_hub import snapshot_download; snapshot_download(repo_id=sys.argv[1], repo_type=sys.argv[2])' \"$_repo_id\" \"$_repo_type\"; fi\n")
	b.WriteString("}\n")
	// hf_validate_token pings /whoami once when HF_TOKEN is set. huggingface-hub
	// v1.x surfaces 401/403 on token-bearing requests as a misleading "not
	// found" error per model; this gives an honest single diagnostic and drops
	// a bad token so anonymous downloads of public models can still succeed.
	b.WriteString("hf_validate_token() {\n")
	b.WriteString("  if [ -z \"${HF_TOKEN:-}\" ]; then return 0; fi\n")
	b.WriteString("  export HF_DEBUG=1\n")
	b.WriteString("  local _ok=0\n")
	b.WriteString("  if command -v hf >/dev/null 2>&1; then if hf auth whoami >/dev/null 2>&1; then _ok=1; fi; elif command -v huggingface-cli >/dev/null 2>&1; then if huggingface-cli whoami >/dev/null 2>&1; then _ok=1; fi; else return 0; fi\n")
	b.WriteString("  if [ \"$_ok\" -ne 1 ]; then echo 'weft: HF_TOKEN is set but rejected by the Hub — check expiry/scope. Unsetting HF_TOKEN and retrying anonymously.' >&2; unset HF_TOKEN HUGGING_FACE_HUB_TOKEN HF_HUB_TOKEN; fi\n")
	b.WriteString("}\n")
	b.WriteString("ensure_hf_download_tool\n")
	b.WriteString("hf_validate_token\n")
	for _, asset := range assets {
		repoType := "model"
		if asset.Kind == dataloc.AssetHFDataset {
			repoType = "dataset"
		}
		b.WriteString(fmt.Sprintf("echo 'Downloading HF %s: %s'\n", repoType, asset.ID))
		b.WriteString(fmt.Sprintf("hf_download %s %s\n", shellQuoteLocal(repoType), shellQuoteLocal(asset.ID)))
	}
	b.WriteString(`find "${HF_HOME:-/root/.cache/huggingface}" -type l ! -exec test -e {} \; -delete 2>/dev/null || true` + "\n")
	return b.String()
}

func shellQuoteLocal(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func (m *setupPrewarmManager) waitFor(job cloud.AgentJob) setupPrewarmResult {
	if m == nil {
		return setupPrewarmResult{}
	}
	key := setupPrewarmKey(job)
	m.mu.Lock()
	if result, ok := m.results[key]; ok {
		delete(m.results, key)
		m.mu.Unlock()
		return result
	}
	active := m.active
	if active == nil || active.runID != job.RunID || active.jobID != job.ID {
		m.mu.Unlock()
		return setupPrewarmResult{}
	}
	done := active.done
	m.mu.Unlock()

	result := <-done
	m.mu.Lock()
	delete(m.results, key)
	m.mu.Unlock()
	return result
}

// jobSequenceResult holds the outcome of running a sequence of jobs.
type jobSequenceResult struct {
	FailedJobs         []int64
	AnyFailed          bool
	AnyInfraFailed     bool
	AnyCanceled        bool
	StartedJobCount    int
	CompletionManifest *runner.InstanceCompletionManifest
}

// runJobSequence runs a slice of agent jobs. Slotted non-benchmark manifests
// run concurrently; other groups run sequentially, overlapping post-job uploads
// with the next job's execution. Benchmark jobs act as a barrier — all
// background work must finish before a benchmark job starts.
func runJobSequence(jobs []cloud.AgentJob, cfg jobSequenceConfig) jobSequenceResult {
	if canRunSlottedJobsConcurrently(jobs) {
		return runSlottedJobSequence(jobs, cfg)
	}
	jobs = orderJobsForSetupOverlap(jobs)

	// GPU-shape preflight BEFORE any billable setup / HF prewarm work (wb41):
	// if the provider allocated fewer GPUs than the sequence's multi-GPU jobs
	// request, terminate now as infra_failure instead of discovering the
	// shortfall at the runner's gpu_count_preflight after prewarm spend. The
	// runner check remains as the final availability guard.
	if checkGPUCount(cfg.R2Bucket, cfg.InstanceID, maxRequestedGPUCount(jobs), cfg.SelfDestructCmd) {
		return jobSequenceResult{AnyFailed: true, AnyInfraFailed: true}
	}

	var result jobSequenceResult
	bgm := newBGWorkManager(jobs, cfg.SkipWorkdirDeletion)
	setupPrewarms := newSetupPrewarmManager()
	gpuWarmedUp := false
	canceledAttempts := map[int64]struct{}{}
	var lastPostJobID int64

	drainCancels := func() {
		canceled, err := drainGraceCancelAttemptRequests(cfg.R2Bucket, cfg.InstanceID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "drain cancel-attempts: %v\n", err)
			return
		}
		for id := range canceled {
			canceledAttempts[id] = struct{}{}
		}
	}
	drainCancels()

	// Drain incoming jobs requests while jobs execute, not just between them:
	// the ack is what lets the coordinator confirm a pending move and cancel
	// the source, so it must not wait behind a multi-hour job.
	var newJobsPoller *jobRequestPoller
	if !cfg.DisableJobPolling {
		newJobsPoller = startJobRequestPoller(cfg.R2Bucket, cfg.InstanceID)
		defer newJobsPoller.Stop()
	}

	for i := 0; i < len(jobs); i++ {
		job := jobs[i]
		drainCancels()
		if _, canceled := canceledAttempts[job.RunID]; canceled {
			fmt.Printf("--- Job %d (run %d) canceled by orchestrator; skipping ---\n", job.ID, job.RunID)
			oplog.LogJob(oplog.OpJobFail, job.ID, "", oplog.WithDetailf("attempt %d canceled by orchestrator", job.RunID))
			result.AnyCanceled = true
			continue
		}

		// Benchmark barrier: wait for all background uploads/deletions
		if db.HasBenchmarkTag(job.Tags) {
			bgm.Barrier()
			if lastPostJobID > 0 {
				setSequencePhase(cfg, fmt.Sprintf("post_job_uploads_drained:%d", lastPostJobID), lastPostJobID)
			}
		}

		// See rule SharedWorkdirUploadBarrierBeforeNextJob in
		// specs/job-lifecycle.allium.
		bgm.WaitForUploadsInWorkdir(runner.ExpandTilde(job.Dir))

		// GPU warmup (opt-in via config): prime system-level CUDA caches
		// before the first benchmark job to avoid cold-start bias.
		if cfg.GPUWarmup && job.UsesGPU && db.HasBenchmarkTag(job.Tags) && !gpuWarmedUp {
			runGPUWarmup(cfg.R2Bucket, cfg.PhaseKey, job.ID, cfg.OnPhase)
			gpuWarmedUp = true
		}

		// Check time budget
		if cfg.MaxTime > 0 {
			remaining := cfg.MaxTime - time.Since(cfg.StartTime)
			if remaining <= 0 {
				fmt.Println("Instance time budget exhausted, skipping remaining jobs")
				break
			}
		}

		fmt.Printf("--- Job %d ---\n", job.ID)
		oplog.LogJob(oplog.OpJobStart, job.ID, "", oplog.WithDetailf("cmd=%s", job.Command))

		// Cloud-after gate: if a same-instance producer this job depends on
		// has failed, skip the consumer with a clear reason.
		if skip, reason := checkCloudAfter(cfg.R2Bucket, job, result.FailedJobs); skip {
			fmt.Fprintf(os.Stderr, "job %d skipped: %s\n", job.ID, reason)
			oplog.LogJob(oplog.OpJobFail, job.ID, "", oplog.WithDetail(reason))
			result.AnyFailed = true
			result.FailedJobs = append(result.FailedJobs, job.ID)

			recordEarlyJobFailure(cfg, job, reason, 1)
			continue
		}

		workDir := job.Dir
		// Recover from a missing/empty workdir (e.g. previous campaign-mid
		// cleanup race, manual rm, or aborted source extract) by re-staging
		// from the locally cached source tarball before stageCloudNeeds runs.
		ensureSourceFresh(cfg.R2Bucket, runner.ExpandTilde(workDir))
		if err := stageCloudNeeds(cfg.R2Bucket, job.ID, workDir, job.CloudNeeds); err != nil {
			fmt.Fprintf(os.Stderr, "cloud artifact staging failed for job %d: %v\n", job.ID, err)
			oplog.LogJob(oplog.OpJobFail, job.ID, "", oplog.WithError(err))
			result.AnyFailed = true
			result.FailedJobs = append(result.FailedJobs, job.ID)

			// Mark job complete with failure even when command did not start.
			exitCode := 1
			failureReason, infra := db.ClassifyInfraFailure(db.PhaseCloudArtifactStaging, exitCode, err.Error())
			if failureReason == "" {
				failureReason = "artifact_stage_failed"
			}
			recordEarlyJobFailure(cfg, job, failureReason, exitCode)
			if infra && cfg.SelfDestructCmd != "" && !cfg.ConcurrentSlot {
				terminateInstanceWithReason(
					cfg.R2Bucket, cfg.InstanceID, cfg.SelfDestructCmd,
					fmt.Sprintf("artifact_stage_infra_failure:%d", job.ID),
					job.ID,
					db.TerminationReasonInfraFailure,
					err.Error(),
				)
			}
			continue
		}

		// Write .started marker to R2
		attemptStartUnix := time.Now().Unix()
		r2Put(cfg.R2Bucket, r2keys.JobAttemptStarted(job.ID, job.RunID), fmt.Sprintf("%d", attemptStartUnix))
		outputWindowStartUnix := outputWindowStartForJob(job, attemptStartUnix)
		result.StartedJobCount++
		paths := runner.NewJobPaths(cfg.LogDir, job.ID)
		stopTimeseriesUploader := startTimeseriesUploader(cfg.R2Bucket, job.ID, job.RunID, paths.Timeseries)
		stopTelemetryUploader := startTelemetryUploader(cfg.R2Bucket, job.ID, job.RunID, paths.Telemetry)

		// Compute per-job max time from remaining budget
		var jobMaxTime time.Duration
		if cfg.MaxTime > 0 {
			jobMaxTime = cfg.MaxTime - time.Since(cfg.StartTime)
		}

		jobCfg := singleJobConfigForAgentJob(job, cfg, workDir, jobMaxTime)
		prewarm := setupPrewarms.waitFor(job)
		if !prewarm.ok && prewarm.err == nil && len(hfInputAssets(job.Inputs)) > 0 {
			prewarm = runSetupPrewarm(job, cfg, runner.ExpandTilde(workDir), false)
		}
		if prewarm.ok {
			jobCfg.SetupPrewarmed = prewarm.setupRan
			if prewarm.didWork {
				jobCfg.SetupPrewarmLog = prewarm.logPath
			}
		} else if prewarm.err != nil {
			fmt.Fprintf(os.Stderr, "prewarm for job %d failed: %v%s\n", job.ID, prewarm.err, prewarm.logTail)
			oplog.LogJob(oplog.OpJobFail, job.ID, "", oplog.WithError(prewarm.err))
			result.AnyFailed = true
			result.FailedJobs = append(result.FailedJobs, job.ID)
			stopTimeseriesUploader()
			stopTelemetryUploader()
			r2Delete(cfg.R2Bucket, r2keys.JobAttemptLiveTimeseries(job.ID, job.RunID))
			r2Delete(cfg.R2Bucket, r2keys.JobAttemptLiveTelemetry(job.ID, job.RunID))
			prewarmExitCode := recordPrewarmFailure(cfg, job, prewarm)

			// Setup/HF prewarm failures at this boundary happen before the
			// user command starts. Timeouts and declared-HF input staging
			// failures are infrastructure-side: terminate the rental so the
			// reconciler resets affected jobs to queued and autopilot places
			// them on a fresh instance. See specs/job-lifecycle.allium.
			if prewarmShouldTerminateAsInfra(prewarm) && cfg.SelfDestructCmd != "" && !cfg.ConcurrentSlot {
				slog.Warn("prewarm infrastructure failure: terminating instance as infra_failure",
					"component", "agent",
					"job_id", job.ID,
					"instance_id", cfg.InstanceID,
					"error", prewarm.err)
				terminateInstanceWithReason(
					cfg.R2Bucket, cfg.InstanceID, cfg.SelfDestructCmd,
					fmt.Sprintf("prewarm_infra_failure:%d", job.ID),
					job.ID,
					db.TerminationReasonInfraFailure,
					prewarm.err.Error(),
				)
			}

			// Schedule the same post-job background work the normal path
			// uses so the prewarm log (now under LogDir for this job)
			// reaches R2. Without this the prewarm traceback dies with
			// the instance — see r2upload.Drain for the upload gate.
			uploadOpslog(cfg.R2Bucket, cfg.InstanceID, cfg.LogDir)
			uploadStartedUnix := time.Now().Unix()
			logSnapshot, snapErr := snapshotLogDir(cfg.LogDir, job.ID, job.RunID)
			if snapErr != nil {
				fmt.Fprintf(os.Stderr, "snapshot log dir for prewarm-failed job %d: %v\n", job.ID, snapErr)
			}
			uploadingPhase := fmt.Sprintf("uploading:%d", job.ID)
			bgm.StartPostJobWork(postJobWork{
				r2Bucket:              cfg.R2Bucket,
				instanceID:            cfg.InstanceID,
				jobID:                 job.ID,
				runID:                 job.RunID,
				exitCode:              prewarmExitCode,
				workDir:               runner.ExpandTilde(workDir),
				logSnapshot:           logSnapshot,
				diskPath:              cfg.DiskPath,
				phase:                 uploadingPhase,
				uploadStartedUnix:     uploadStartedUnix,
				outputWindowStartUnix: outputWindowStartUnix,
				outputDirs:            job.OutputDirs,
			})
			if cfg.OnPhase != nil {
				cfg.OnPhase(uploadingPhase)
			}
			writePhase(cfg.R2Bucket, cfg.PhaseKey, uploadingPhase)
			lastPostJobID = job.ID
			if i < len(jobs)-1 {
				cleanLogDir(cfg.LogDir)
				oplog.Init(filepath.Join(cfg.LogDir, agentOpslogFile), 0)
			}
			continue
		}
		originalOnPhase := jobCfg.OnPhase
		jobCfg.OnPhase = func(phase string) {
			if originalOnPhase != nil {
				originalOnPhase(phase)
			}
			if phase == "running" {
				setupPrewarms.startNext(i, jobs, cfg, workDir)
			}
		}

		ei, err := runJobWithProgress(cfg.R2Bucket, job.ID, job.RunID, cfg.InstanceID, cfg.LogDir, jobCfg, outputWindowStartUnix)
		if job.UsesGPU {
			gpuWarmedUp = true
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "run-job %d failed: %v\n", job.ID, err)
			oplog.LogJob(oplog.OpJobFail, job.ID, "", oplog.WithError(err))
			result.AnyFailed = true
			result.FailedJobs = append(result.FailedJobs, job.ID)
		} else if ei.ExitCode != 0 {
			fmt.Printf("Job %d failed (exit %d)\n", job.ID, ei.ExitCode)
			oplog.LogJob(oplog.OpJobFail, job.ID, "", oplog.WithDetailf("exit=%d", ei.ExitCode))
			result.AnyFailed = true
			result.FailedJobs = append(result.FailedJobs, job.ID)
		} else {
			fmt.Printf("Job %d completed successfully\n", job.ID)
			oplog.LogJob(oplog.OpJobComplete, job.ID, "", oplog.WithDetail("exit=0"))
		}

		// Compute the exit code that will be written to .complete and used in
		// any fallback completion record. err != nil from runJobWithProgress
		// means the runner returned early without finishing normally — treat
		// as exit=1 if it didn't already give us a non-zero code.
		exitCode := ei.ExitCode
		if err != nil && exitCode == 0 {
			exitCode = 1
		}

		// Defense in depth: if the runner returned an error and didn't leave a
		// failure_reason / completion.json behind, synthesize stubs so the
		// "why" reaches local sync (and the DB) instead of being silently
		// dropped. Without this, a runner-level start failure produces an
		// instance whose status reads "exit 1" with nothing else.
		ensureFailureArtifacts(cfg.LogDir, job.ID, ei, err)
		if jobFailureReasonIsInfra(cfg.LogDir, job.ID) {
			result.AnyInfraFailed = true
		}

		// Upload the opslog before writing .complete so a self-destruct that
		// races the post-job work still leaves the agent's diagnostic trail
		// (which captures the stderr message from runJobWithProgress) in R2.
		uploadOpslog(cfg.R2Bucket, cfg.InstanceID, cfg.LogDir)

		// Write .complete marker synchronously so local sync sees
		// this job as finished before the next job's .started marker.
		r2Put(cfg.R2Bucket, r2keys.JobAttemptComplete(job.ID, job.RunID), fmt.Sprintf("%d", exitCode))

		// === Synchronous post-job work ===
		cleanupPhase := fmt.Sprintf("post_job_cleanup:%d", job.ID)
		setSequencePhase(cfg, cleanupPhase, job.ID)

		uploadStartedUnix := time.Now().Unix()
		if err := patchPhaseUploadWindow(cfg.LogDir, job.ID, uploadStartedUnix, 0); err != nil {
			fmt.Fprintf(os.Stderr, "patch phase timing for job %d: upload start: %v\n", job.ID, err)
		}

		// Stop per-job live uploaders before next job starts its own
		stopTimeseriesUploader()
		stopTelemetryUploader()
		r2Delete(cfg.R2Bucket, r2keys.JobAttemptLiveTimeseries(job.ID, job.RunID))
		r2Delete(cfg.R2Bucket, r2keys.JobAttemptLiveTelemetry(job.ID, job.RunID))

		promoteUVManifest(cfg.R2Bucket, cfg.LogDir)
		uploadOpslog(cfg.R2Bucket, cfg.InstanceID, cfg.LogDir)

		// Snapshot log dir so background uploads can read from it
		// while the live log dir is cleaned for the next job
		logSnapshot, snapErr := snapshotLogDir(cfg.LogDir, job.ID, job.RunID)
		if snapErr != nil {
			fmt.Fprintf(os.Stderr, "snapshot log dir for job %d: %v\n", job.ID, snapErr)
		}

		if i < len(jobs)-1 {
			cleanLogDir(cfg.LogDir)
			oplog.Init(filepath.Join(cfg.LogDir, agentOpslogFile), 0)
		}

		// Update phase so TUI shows "uploading" during background uploads
		uploadingPhase := fmt.Sprintf("uploading:%d", job.ID)

		// === Background post-job work (uploads + workdir cleanup) ===
		bgm.StartPostJobWork(postJobWork{
			r2Bucket:              cfg.R2Bucket,
			instanceID:            cfg.InstanceID,
			jobID:                 job.ID,
			runID:                 job.RunID,
			exitCode:              exitCode,
			workDir:               runner.ExpandTilde(workDir),
			logSnapshot:           logSnapshot,
			diskPath:              cfg.DiskPath,
			phase:                 uploadingPhase,
			outputWindowStartUnix: outputWindowStartUnix,
			outputDirs:            job.OutputDirs,
			uploadStartedUnix:     uploadStartedUnix,
		})

		if cfg.OnPhase != nil {
			cfg.OnPhase(uploadingPhase)
		}
		writePhase(cfg.R2Bucket, cfg.PhaseKey, uploadingPhase)
		lastPostJobID = job.ID

		// Merge jobs accepted since the last merge (mid-job drains plus one
		// final synchronous drain) into the sequence.
		setSequencePhase(cfg, "ready_for_next_job", job.ID)
		if newJobsPoller == nil {
			continue
		}
		if newJobs := newJobsPoller.Take(func(phase string) {
			if cfg.OnPhase != nil {
				cfg.OnPhase(phase)
			}
			writePhase(cfg.R2Bucket, cfg.PhaseKey, phase)
		}); len(newJobs) > 0 {
			fmt.Printf("Picked up %d new job(s) from R2\n", len(newJobs))
			bgm.RegisterNewJobs(newJobs)
			jobs = append(jobs, newJobs...)
		}
	}

	// Wait for remaining background work, then clean up workdirs.
	// Cleanup is intentionally deferred to here (after the new-job pickup
	// loop has exited and the poller is stopped) to avoid racing a source
	// extract against workdir deletion.
	if newJobsPoller != nil {
		newJobsPoller.Stop()
	}
	bgm.Barrier()
	if lastPostJobID > 0 {
		setSequencePhase(cfg, fmt.Sprintf("post_job_uploads_drained:%d", lastPostJobID), lastPostJobID)
	}
	result.CompletionManifest = collectCompletionManifest(cfg.LogDir, jobs, bgm.CompletionSummaries()...)
	bgm.CleanupWorkdirs()
	return result
}

func canRunSlottedJobsConcurrently(jobs []cloud.AgentJob) bool {
	if len(jobs) < 2 {
		return false
	}
	seen := map[string]bool{}
	jobIDs := make(map[int64]bool, len(jobs))
	for _, job := range jobs {
		jobIDs[job.ID] = true
	}
	for _, job := range jobs {
		if !job.SlotGPU {
			return false
		}
		gpu := strings.TrimSpace(job.GPU)
		if gpu == "" {
			return false
		}
		if strings.Contains(gpu, ",") {
			return false
		}
		if _, err := strconv.Atoi(gpu); err != nil {
			return false
		}
		if db.HasBenchmarkTag(job.Tags) || agentJobHasTag(job.Tags, db.TagExclusive) {
			return false
		}
		if job.GPUCount > 1 {
			return false
		}
		if seen[gpu] {
			return false
		}
		seen[gpu] = true
		for _, ref := range job.CloudAfter {
			if jobIDs[ref.JobID] {
				return false
			}
		}
	}
	return true
}

func agentJobHasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if db.CanonicalizeTag(t) == tag {
			return true
		}
	}
	return false
}

func runSlottedJobSequence(jobs []cloud.AgentJob, cfg jobSequenceConfig) jobSequenceResult {
	if checkGPUCount(cfg.R2Bucket, cfg.InstanceID, requiredSlottedGPUCount(jobs), cfg.SelfDestructCmd) {
		return jobSequenceResult{AnyFailed: true, AnyInfraFailed: true}
	}
	fmt.Printf("--- Running %d slotted jobs concurrently ---\n", len(jobs))
	setSequencePhase(cfg, fmt.Sprintf("running_slotted:%d", len(jobs)), 0)
	var newJobsPoller *jobRequestPoller
	if !cfg.DisableJobPolling {
		newJobsPoller = startJobRequestPoller(cfg.R2Bucket, cfg.InstanceID)
	}

	type slotResult struct {
		result jobSequenceResult
	}
	ch := make(chan slotResult, len(jobs))
	for _, job := range jobs {
		job := job
		fatalAgentGo(fmt.Sprintf("slotted-job-%d", job.ID), func() {
			slotCfg := cfg
			slotCfg.DisableJobPolling = true
			slotCfg.SkipWorkdirDeletion = true
			slotCfg.ConcurrentSlot = true
			ch <- slotResult{result: runJobSequence([]cloud.AgentJob{job}, slotCfg)}
		})
	}

	var result jobSequenceResult
	summaries := make([]runner.JobCompletionSummary, 0, len(jobs))
	for range jobs {
		r := (<-ch).result
		mergeJobSequenceResult(&result, r, &summaries)
	}
	if newJobsPoller != nil {
		newJobs := newJobsPoller.Take(func(phase string) {
			if cfg.OnPhase != nil {
				cfg.OnPhase(phase)
			}
			writePhase(cfg.R2Bucket, cfg.PhaseKey, phase)
		})
		newJobsPoller.Stop()
		newJobsPoller = nil
		if len(newJobs) > 0 {
			fmt.Printf("Picked up %d new job(s) after slotted jobs drained\n", len(newJobs))
			followup := runJobSequence(newJobs, cfg)
			mergeJobSequenceResult(&result, followup, &summaries)
			jobs = append(jobs, newJobs...)
		}
	}
	sort.Slice(result.FailedJobs, func(i, j int) bool { return result.FailedJobs[i] < result.FailedJobs[j] })
	result.CompletionManifest = collectCompletionManifest(cfg.LogDir, jobs, summaries...)
	cleanupSlottedWorkdirs(jobs, cfg.SkipWorkdirDeletion)
	return result
}

func mergeJobSequenceResult(dst *jobSequenceResult, src jobSequenceResult, summaries *[]runner.JobCompletionSummary) {
	dst.FailedJobs = append(dst.FailedJobs, src.FailedJobs...)
	dst.AnyFailed = dst.AnyFailed || src.AnyFailed
	dst.AnyInfraFailed = dst.AnyInfraFailed || src.AnyInfraFailed
	dst.AnyCanceled = dst.AnyCanceled || src.AnyCanceled
	dst.StartedJobCount += src.StartedJobCount
	if src.CompletionManifest != nil {
		*summaries = append(*summaries, src.CompletionManifest.Jobs...)
	}
}

func cleanupSlottedWorkdirs(jobs []cloud.AgentJob, skip bool) {
	if skip {
		return
	}
	seen := map[string]bool{}
	for _, job := range jobs {
		if !job.SlotGPU {
			continue
		}
		dir := runner.ExpandTilde(job.Dir)
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("delete slotted workdir", "component", "agent", "job_id", job.ID, "path", dir, "error", err)
		}
	}
}

func requiredSlottedGPUCount(jobs []cloud.AgentJob) int {
	required := len(jobs)
	for _, job := range jobs {
		idx, err := strconv.Atoi(strings.TrimSpace(job.GPU))
		if err == nil && idx+1 > required {
			required = idx + 1
		}
	}
	return required
}

func jobFailureReasonIsInfra(logDir string, jobID int64) bool {
	reason := runner.ReadFailureReasonFile(runner.NewJobPaths(logDir, jobID).FailureReason)
	return db.IsInfraFailureReason(reason)
}

func setSequencePhase(cfg jobSequenceConfig, phase string, jobID int64) {
	if cfg.OnPhase != nil {
		cfg.OnPhase(phase)
	}
	oplog.Log(oplog.OpPhaseTransition, oplog.WithJobID(jobID), oplog.WithDetail(phase))
	writePhase(cfg.R2Bucket, cfg.PhaseKey, phase)
}

// recordPrewarmFailure writes failure artifacts and the R2 .complete marker
// for a job whose setup prewarm failed. It returns the exit code it recorded
// so callers (e.g. the background-upload repair path) reuse the same value
// instead of clobbering it.
func recordPrewarmFailure(cfg jobSequenceConfig, job cloud.AgentJob, prewarm setupPrewarmResult) int {
	paths := runner.NewJobPaths(cfg.LogDir, job.ID)
	_ = os.MkdirAll(cfg.LogDir, 0o755)
	if err := runner.ArchiveExistingFiles(cfg.LogDir, job.ID); err != nil {
		slog.Warn("archive prior artifacts before prewarm-failure write", "component", "agent", "job_id", job.ID, "error", err)
	}
	now := time.Now().Unix()
	_ = runner.WriteMetaFile(paths, job.ID, job.Dir, job.Command, "", now, "")
	_ = runner.WriteLogHeader(paths, job.ID, job.Dir, job.Command, "")
	if prewarm.logPath != "" {
		appendPrewarmLogForFailure(paths.Log, prewarm.logPath)
	}
	reason := "prewarm_failed"
	if prewarm.failureReason != "" {
		reason = prewarm.failureReason
	} else if prewarm.err != nil {
		reason = prewarm.err.Error()
	}
	ei := prewarm.exitInfo
	ei.ExitCode = prewarmFailureExitCode(prewarm)
	_ = runner.WriteStatusFile(paths, ei)
	_ = runner.WriteFailureReasonFile(paths, reason)
	_ = runner.WriteCompletionRecord(paths, ei, runner.RunningJobState{}, "", reason, now, now, nil)
	_ = runner.WritePhasesFile(paths, runner.PhaseTiming{
		WrapperStart: now,
		SetupStart:   now,
		SetupEnd:     now,
	})
	r2Put(cfg.R2Bucket, r2keys.JobAttemptComplete(job.ID, job.RunID), fmt.Sprintf("%d", ei.ExitCode))
	return ei.ExitCode
}

func prewarmShouldTerminateAsInfra(prewarm setupPrewarmResult) bool {
	if prewarm.infraFailure {
		return true
	}
	_, infra := db.ClassifyInfraFailure(db.PhaseSetup, prewarm.exitInfo.ExitCode, "")
	return infra
}

// prewarmFailureExitCode preserves the underlying RunSetupCommand exit code
// when present so downstream classification can distinguish exit 124
// (setup-phase timeout, infrastructure-side) from a generic exit 1 (code
// error). A zero exit code (prewarm failed without running a command) maps
// to 1.
func prewarmFailureExitCode(prewarm setupPrewarmResult) int {
	if prewarm.exitInfo.ExitCode == 0 {
		return 1
	}
	return prewarm.exitInfo.ExitCode
}

func recordEarlyJobFailure(cfg jobSequenceConfig, job cloud.AgentJob, reason string, exitCode int) {
	if exitCode == 0 {
		exitCode = 1
	}
	ei := runner.ExitInfo{ExitCode: exitCode}
	paths := runner.NewJobPaths(cfg.LogDir, job.ID)
	_ = os.MkdirAll(cfg.LogDir, 0o755)
	_ = runner.WriteStatusFile(paths, ei)
	_ = runner.WriteFailureReasonFile(paths, reason)
	now := time.Now().Unix()
	_ = runner.WriteCompletionRecord(paths, ei, runner.RunningJobState{}, "", reason, now, now, nil)

	if cfg.R2Bucket != "" {
		upload := uploadJobResultsForAgent(cfg.R2Bucket, job.ID, job.RunID, cfg.LogDir)
		if upload.Status != "ok" {
			fmt.Fprintf(os.Stderr, "upload results for failed job %d: %s %s\n", job.ID, upload.Status, upload.Error)
		}
	}
	_ = r2PutForAgent(cfg.R2Bucket, r2keys.JobAttemptComplete(job.ID, job.RunID), fmt.Sprintf("%d", exitCode))
}

func appendPrewarmLogForFailure(jobLog, prewarmLog string) {
	data, err := os.ReadFile(prewarmLog)
	if err != nil || len(data) == 0 {
		return
	}
	f, err := os.OpenFile(jobLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("\n=== PREWARM LOG ===\n")
	_, _ = f.Write(data)
	if data[len(data)-1] != '\n' {
		_, _ = f.WriteString("\n")
	}
	_, _ = f.WriteString("=== END PREWARM LOG ===\n")
}

func singleJobConfigForAgentJob(job cloud.AgentJob, cfg jobSequenceConfig, workDir string, jobMaxTime time.Duration) runner.SingleJobConfig {
	gpuIdle, stdoutSilence := pickWatchdogTimeouts(cfg.CostPerHourCents)
	setupTimeout := pickSetupTimeout(cfg)
	env := agentRentalEnv(cfg)
	env = appendAgentJobEnv(env, job)
	var gpuMem *int
	if job.GPUMemGB > 0 {
		v := job.GPUMemGB
		gpuMem = &v
	}
	jobCfg := runner.SingleJobConfig{
		JobID: job.ID,
		Job: opsqueue.CommandJob{
			Cmd:          job.Command,
			Tags:         append([]string(nil), job.Tags...),
			GPU:          job.GPU,
			GPUClass:     job.GPUClass,
			GPUCount:     job.GPUCount,
			GPUMem:       gpuMem,
			Interconnect: job.Interconnect,
			CPUCores:     job.CPUCores,
			OutputDirs:   append([]string(nil), job.OutputDirs...),
			Outputs:      append([]string(nil), job.Outputs...),
			Produces:     append([]string(nil), job.Produces...),
			Needs:        append([]string(nil), job.Needs...),
			Env:          env,
		},
		LogDir:               cfg.LogDir,
		WorkingDir:           workDir,
		MaxTime:              jobMaxTime,
		SetupTimeout:         setupTimeout,
		GPUIdleTimeout:       gpuIdle,
		StdoutSilenceTimeout: stdoutSilence,
		OnPhase:              phaseCallback(cfg.R2Bucket, cfg.PhaseKey, job.ID, cfg.OnPhase),
	}
	if strings.TrimSpace(job.GPU) != "" {
		devices := strings.Split(job.GPU, ",")
		jobCfg.GPUActiveProbe = func() bool { return runner.GPUDevicesActive(devices) }
	}
	return jobCfg
}

func patchCompletionUpload(logDir string, jobID int64, upload *runner.OutputUploadResult) {
	patchCompletionRecord(logDir, jobID, func(rec *runner.CompletionRecord) {
		rec.OutputUpload = upload
	})
}

func patchCompletionResultsUpload(logDir string, jobID int64, upload *runner.UploadSummary) {
	patchCompletionRecord(logDir, jobID, func(rec *runner.CompletionRecord) {
		rec.ResultsUpload = upload
	})
}

// collectCompletionManifest assembles an InstanceCompletionManifest suitable
// for writing to the R2 completion marker. Summaries from background upload
// workers cover jobs whose shared log directory was cleaned before later jobs;
// scanning logDir covers failures that entered grace before self-destruct.
func collectCompletionManifest(logDir string, jobs []cloud.AgentJob, extra ...runner.JobCompletionSummary) *runner.InstanceCompletionManifest {
	manifest := &runner.InstanceCompletionManifest{
		CompletedAtUnix: time.Now().Unix(),
	}
	summaries := map[int64]runner.JobCompletionSummary{}
	for _, summary := range extra {
		if summary.JobID != 0 {
			summaries[summary.JobID] = summary
		}
	}
	for _, summary := range scanCompletionSummaries(logDir) {
		if summary.JobID != 0 {
			summaries[summary.JobID] = summary
		}
	}

	seen := map[int64]struct{}{}
	allOK := true
	for _, job := range jobs {
		summary, ok := summaries[job.ID]
		if !ok {
			summary, ok = summarizeJobCompletion(logDir, job.ID)
		}
		if !completionSummaryOK(summary, ok) {
			allOK = false
		}
		manifest.Jobs = append(manifest.Jobs, summary)
		seen[job.ID] = struct{}{}
	}

	var remaining []int64
	for jobID := range summaries {
		if _, ok := seen[jobID]; !ok {
			remaining = append(remaining, jobID)
		}
	}
	sort.Slice(remaining, func(i, j int) bool { return remaining[i] < remaining[j] })
	for _, jobID := range remaining {
		summary := summaries[jobID]
		if !completionSummaryOK(summary, true) {
			allOK = false
		}
		manifest.Jobs = append(manifest.Jobs, summary)
	}
	if !allOK {
		manifest.ExitCode = 1
	}
	return manifest
}

func completionSummaryOK(summary runner.JobCompletionSummary, readable bool) bool {
	return readable && summary.ExitCode == 0
}

func scanCompletionSummaries(logDir string) []runner.JobCompletionSummary {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return nil
	}
	summaries := make([]runner.JobCompletionSummary, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		jobIDText, ok := strings.CutSuffix(name, ".completion.json")
		if !ok || strings.Contains(jobIDText, "-") {
			continue
		}
		jobID, err := strconv.ParseInt(jobIDText, 10, 64)
		if err != nil || jobID <= 0 {
			continue
		}
		summary, _ := summarizeJobCompletion(logDir, jobID)
		if summary.JobID != 0 {
			summaries = append(summaries, summary)
		}
	}
	return summaries
}

// summarizeJobCompletion reads a job's completion record and returns a summary.
// Returns (summary, ok) where ok is false if the job failed or the record is unreadable.
func summarizeJobCompletion(logDir string, jobID int64) (runner.JobCompletionSummary, bool) {
	paths := runner.NewJobPaths(logDir, jobID)
	data, err := os.ReadFile(paths.Completion)
	if err != nil {
		return runner.JobCompletionSummary{JobID: jobID, ExitCode: -1, UploadStatus: "unknown"}, false
	}
	var rec runner.CompletionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return runner.JobCompletionSummary{JobID: jobID, ExitCode: -1, UploadStatus: "unknown"}, false
	}
	uploadStatus := "ok"
	if rec.OutputUpload != nil && rec.OutputUpload.Status != "ok" {
		uploadStatus = rec.OutputUpload.Status
	}
	if rec.ResultsUpload != nil && rec.ResultsUpload.Status != "ok" {
		if uploadStatus == "ok" {
			uploadStatus = rec.ResultsUpload.Status
		} else {
			uploadStatus = "partial"
		}
	}
	var outputBytes int64
	if rec.OutputUpload != nil {
		outputBytes = rec.OutputUpload.Bytes
	}
	return runner.JobCompletionSummary{
		JobID:        jobID,
		ExitCode:     rec.ExitCode,
		UploadStatus: uploadStatus,
		OutputBytes:  outputBytes,
	}, rec.ExitCode == 0
}

func patchCompletionRecord(logDir string, jobID int64, mutate func(*runner.CompletionRecord)) {
	if mutate == nil {
		return
	}
	paths := runner.NewJobPaths(logDir, jobID)
	data, err := os.ReadFile(paths.Completion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "patch completion record for job %d: read: %v\n", jobID, err)
		return
	}

	var rec runner.CompletionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		fmt.Fprintf(os.Stderr, "patch completion record for job %d: parse: %v\n", jobID, err)
		return
	}

	mutate(&rec)
	out, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "patch completion record for job %d: encode: %v\n", jobID, err)
		return
	}
	out = append(out, '\n')
	if err := os.WriteFile(paths.Completion, out, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "patch completion record for job %d: write: %v\n", jobID, err)
	}
}

func patchPhaseUploadWindow(logDir string, jobID, uploadStart, uploadEnd int64) error {
	paths := runner.NewJobPaths(logDir, jobID)
	data, err := os.ReadFile(paths.Phases)
	if err != nil {
		return err
	}

	var phases runner.PhaseTiming
	if err := json.Unmarshal(data, &phases); err != nil {
		return err
	}
	if uploadStart > 0 {
		phases.UploadStart = uploadStart
	}
	if uploadEnd > 0 {
		phases.UploadEnd = uploadEnd
	}

	out, err := json.MarshalIndent(phases, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(paths.Phases, out, 0o644)
}

// snapshotLogDir copies the log directory contents to a temp dir so that
// the main log dir can be cleaned for the next job while background uploads
// read from the snapshot. The caller must os.RemoveAll the returned path.
//
// The snapshot dir is keyed by job ID + run ID (like the setup-prewarm dir)
// so a later attempt of the same job on the same instance gets its own
// directory: the previous attempt's background upload ends with an
// os.RemoveAll of its snapshot, which must not delete or be fed files from
// the new attempt.
func snapshotLogDir(logDir string, jobID, runID int64) (string, error) {
	snapshot := filepath.Join(os.TempDir(), fmt.Sprintf("weft-logs-job-%d-%d", jobID, runID))
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return snapshot, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue // log dir is flat
		}
		name := entry.Name()
		src := filepath.Join(logDir, name)
		dst := filepath.Join(snapshot, name)
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		os.WriteFile(dst, data, 0o644)
	}
	return snapshot, nil
}

// ensureFailureArtifacts writes a stub failure_reason and completion.json when
// the runner returned an error or non-zero exit code without leaving them
// behind itself. This catches early returns from RunSingleJob (e.g. setup
// failures that didn't reach the normal completion path) so local sync
// has something to ingest into job_attempts.failure_reason instead of
// surfacing only "exit 1" with no diagnosis.
func ensureFailureArtifacts(logDir string, jobID int64, ei runner.ExitInfo, runErr error) {
	if runErr == nil && ei.ExitCode == 0 {
		return
	}
	paths := runner.NewJobPaths(logDir, jobID)

	reason := ""
	if runErr != nil {
		reason = runErr.Error()
	}
	if reason == "" {
		reason = runner.DetectFailureReasonFromExitInfo(ei)
	}

	if existing := runner.ReadFailureReasonFile(paths.FailureReason); existing == "" {
		_ = runner.WriteFailureReasonFile(paths, reason)
	}

	if _, err := os.Stat(paths.Completion); err != nil {
		exitCode := ei.ExitCode
		if exitCode == 0 {
			exitCode = 1
		}
		now := time.Now().Unix()
		_ = runner.WriteCompletionRecord(paths, runner.ExitInfo{ExitCode: exitCode}, runner.RunningJobState{}, "", reason, now, now, nil)
	}
}

// runGPUWarmup runs a lightweight Python command to prime the CUDA context
// (context init, cuBLAS handle, memory allocator) so that benchmark jobs
// don't pay cold-start overhead in their first measured config.
func runGPUWarmup(r2Bucket, phaseKey string, nextJobID int64, onPhase func(string)) {
	phase := fmt.Sprintf("gpu_warmup:%d", nextJobID)
	if onPhase != nil {
		onPhase(phase)
	}
	writePhase(r2Bucket, phaseKey, phase)
	fmt.Println("Running GPU warmup (CUDA context + cuBLAS init)...")

	start := time.Now()
	cmd := exec.Command("python3", "-c",
		"import torch; torch.zeros(1, device='cuda'); torch.mm(torch.randn(2,2, device='cuda'), torch.randn(2,2, device='cuda'))")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "GPU warmup failed: %v (benchmark measurements may include cold-start overhead)\n", err)
	} else {
		fmt.Printf("GPU warmup completed in %s\n", time.Since(start).Round(time.Millisecond))
	}
}

// checkCloudAfter returns (skip=true, reason) when a CloudAfter-referenced
// producer has not confirmably completed successfully.
//
// A producer observed to fail within this same agent session (present in
// failedJobs) is treated as failed without an extra round trip. Otherwise,
// completion is verified positively via the producer's R2 completion marker
// (r2keys.JobAttemptComplete, written by both the success and failure paths
// in runJobSequence) rather than inferred from absence in failedJobs.
//
// The old "absence of evidence" check treated "producer not observed to
// fail" as "satisfied" -- but that's indistinguishable from "producer was
// assigned to this same launch but never actually got dispatched" (still
// queued in the DB) or "producer's attempt was canceled by the
// orchestrator". Both looked like "completed in a prior session" from this
// agent's point of view. wj4663/wj4665 incident: the declared producer sat
// queued on the same launch_id as the consumer, and the consumer ran anyway
// because it was never observed to fail. Requiring a completion marker
// closes that gap while still treating genuinely prior-session completions
// (e.g. before a grace-wake) as satisfied, since those write the marker too.
//
// AllowFailure refs skip the failure check but still require a completion
// marker to exist -- an unmarked producer means "unknown", not "failure is
// fine".
func checkCloudAfter(bucket string, job cloud.AgentJob, failedJobs []int64) (bool, string) {
	if len(job.CloudAfter) == 0 {
		return false, ""
	}
	failed := make(map[int64]bool, len(failedJobs))
	for _, id := range failedJobs {
		failed[id] = true
	}
	for _, ref := range job.CloudAfter {
		if failed[ref.JobID] {
			if ref.AllowFailure {
				continue
			}
			return true, fmt.Sprintf("cloud_after_failed: producer job %d failed on this instance", ref.JobID)
		}
		exitCode, ok, err := producerCompletionExitCode(bucket, ref.JobID, ref.RunID)
		if err != nil {
			// R2 unreachable or an unparseable marker: don't wedge the
			// whole sequence on a transient issue we can't resolve here.
			fmt.Fprintf(os.Stderr, "cloud_after completion check for producer %d: %v\n", ref.JobID, err)
			continue
		}
		if !ok {
			// AllowFailure tolerates any *terminal* outcome (mirrors
			// --after-any in dependencySatisfied, cmd/instance_launch.go) --
			// it does not tolerate "unknown", so this still gates.
			return true, fmt.Sprintf("cloud_after_incomplete: producer job %d run %d has not completed", ref.JobID, ref.RunID)
		}
		if exitCode != 0 && !ref.AllowFailure {
			return true, fmt.Sprintf("cloud_after_failed: producer job %d failed (exit %d)", ref.JobID, exitCode)
		}
	}
	return false, ""
}

// producerCompletionExitCode reads the R2 completion marker for a specific
// job attempt. ok=false means no marker exists yet (attempt not started, not
// yet finished, or never dispatched) -- distinct from an error, which means
// the check itself could not be completed.
func producerCompletionExitCode(bucket string, jobID, runID int64) (exitCode int, ok bool, err error) {
	content, err := graceR2Get(bucket, r2keys.JobAttemptComplete(jobID, runID))
	if err != nil {
		return 0, false, err
	}
	if content == "" {
		return 0, false, nil
	}
	code, convErr := strconv.Atoi(content)
	if convErr != nil {
		return 0, false, fmt.Errorf("parse completion marker for job %d run %d: %q: %w", jobID, runID, content, convErr)
	}
	return code, true, nil
}
