package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/opsqueue"
)

// ExitInfo captures detailed exit information from a process, including signal data.
type ExitInfo struct {
	ExitCode int
	Signaled bool
	Signal   syscall.Signal
	CoreDump bool
}

// ExtractExitInfo extracts signal information from a process exit error.
func ExtractExitInfo(err error) ExitInfo {
	if err == nil {
		return ExitInfo{ExitCode: 0}
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		return ExitInfo{ExitCode: 1}
	}
	info := ExitInfo{ExitCode: exitErr.ExitCode()}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			info.Signaled = true
			info.Signal = ws.Signal()
			info.CoreDump = ws.CoreDump()
		}
	}
	return info
}

// SignalName returns the signal name (e.g. "SIGKILL") or empty string if not signaled.
// Failure reasons are reported separately by DetectFailureReasonFromExitInfo
// (e.g. "disk_full", "oom", "gpu_oom").
func (ei ExitInfo) SignalName() string {
	if !ei.Signaled {
		return ""
	}
	return ei.Signal.String()
}

// Hang-watchdog exit codes and labels. See specs/job-lifecycle.allium
// rules GPUIdleKillsJob and StdoutSilenceKillsJob.
const (
	ExitCodeGPUIdleKill        = 125
	ExitCodeStdoutSilenceKill  = 126
	KillReasonGPUIdle          = "gpu-idle"
	KillReasonStdoutSilence    = "stdout-silence"
	FailureReasonGPUIdle       = "killed_gpu_idle"
	FailureReasonStdoutSilence = "killed_stdout_silence"
)

// DetectFailureReasonFromExitInfo examines exit info and system state to determine why a job failed.
func DetectFailureReasonFromExitInfo(ei ExitInfo) string {
	// Watchdog kills synthesize a specific exit code alongside SIGTERM;
	// check exit code first so they aren't misclassified as killed_sigterm.
	switch ei.ExitCode {
	case ExitCodeGPUIdleKill:
		return FailureReasonGPUIdle
	case ExitCodeStdoutSilenceKill:
		return FailureReasonStdoutSilence
	}
	if ei.Signaled {
		if checkDiskFull() {
			return "disk_full"
		}
		switch ei.Signal {
		case syscall.SIGKILL:
			if checkDmesgOOM() {
				return "oom"
			}
			return "killed_sigkill"
		case syscall.SIGSEGV:
			return "segfault"
		case syscall.SIGTERM:
			return "killed_sigterm"
		case syscall.SIGABRT:
			return "aborted"
		default:
			return fmt.Sprintf("signal_%s", strings.ToLower(ei.Signal.String()))
		}
	}
	return DetectFailureReason(ei.ExitCode)
}

// JobPaths holds all file paths for a job.
type JobPaths struct {
	Log           string
	Status        string
	Meta          string
	PID           string
	PGID          string
	Samples       string
	Paused        string
	Rusage        string
	FailureReason string
	Timeseries    string
	Telemetry     string
	KillReason    string
	Heartbeat     string
	Completion    string
	Phases        string
	ManifestError string
}

// TimeseriesSample holds a single time-series telemetry sample for a running job.
type TimeseriesSample struct {
	Ts             int64  `json:"ts"`
	CPUPct         int    `json:"cpu_pct"`
	RSSKB          int64  `json:"rss_kb"`
	GPUMiB         int    `json:"gpu_mib,omitempty"`
	DiskFreeBytes  int64  `json:"disk_free_bytes,omitempty"`
	DiskTotalBytes int64  `json:"disk_total_bytes,omitempty"`
	HostRSSKB      int64  `json:"host_rss_kb,omitempty"`
	HostMemTotal   int64  `json:"host_mem_total_kb,omitempty"`
	GPUUtilPct     int    `json:"gpu_util_pct,omitempty"`
	GPUMemUsed     int    `json:"gpu_mem_used_mib,omitempty"`
	GPUMemTotal    int    `json:"gpu_mem_total_mib,omitempty"`
	GPUTempC       int    `json:"gpu_temp_c,omitempty"`
	GPUClockMHz    int    `json:"gpu_clock_mhz,omitempty"`
	MemPressure    string `json:"mem_pressure,omitempty"`
	Tenant         string `json:"tenant"`
}

// NewJobPaths returns file paths for all job-related files.
func NewJobPaths(logDir string, jobID int64) JobPaths {
	return JobPaths{
		Log:           filepath.Join(logDir, fmt.Sprintf("%d.log", jobID)),
		Status:        filepath.Join(logDir, fmt.Sprintf("%d.status", jobID)),
		Meta:          filepath.Join(logDir, fmt.Sprintf("%d.meta", jobID)),
		PID:           filepath.Join(logDir, fmt.Sprintf("%d.pid", jobID)),
		PGID:          filepath.Join(logDir, fmt.Sprintf("%d.pgid", jobID)),
		Samples:       filepath.Join(logDir, fmt.Sprintf("%d.samples", jobID)),
		Paused:        filepath.Join(logDir, fmt.Sprintf("%d.paused", jobID)),
		Rusage:        filepath.Join(logDir, fmt.Sprintf("%d.rusage", jobID)),
		FailureReason: filepath.Join(logDir, fmt.Sprintf("%d.failure_reason", jobID)),
		Timeseries:    filepath.Join(logDir, fmt.Sprintf("%d.timeseries.jsonl", jobID)),
		Telemetry:     filepath.Join(logDir, fmt.Sprintf("%d.telemetry.jsonl", jobID)),
		KillReason:    filepath.Join(logDir, fmt.Sprintf("%d.kill_reason", jobID)),
		Heartbeat:     filepath.Join(logDir, fmt.Sprintf("%d.heartbeat", jobID)),
		Completion:    filepath.Join(logDir, fmt.Sprintf("%d.completion.json", jobID)),
		Phases:        filepath.Join(logDir, fmt.Sprintf("%d.phases.json", jobID)),
		ManifestError: filepath.Join(logDir, fmt.Sprintf("%d.manifest_error", jobID)),
	}
}

// ArchiveExistingFiles renames existing job files with a timestamp suffix.
func ArchiveExistingFiles(logDir string, jobID int64) {
	extensions := []string{"log", "status", "meta", "pid", "pgid", "samples", "paused", "rusage", "failure_reason", "timeseries.jsonl", "telemetry.jsonl", "kill_reason", "heartbeat", "completion.json", "phases.json", "manifest_error"}
	for _, ext := range extensions {
		path := filepath.Join(logDir, fmt.Sprintf("%d.%s", jobID, ext))
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		ts := info.ModTime().Format("20060102-150405")
		newPath := filepath.Join(logDir, fmt.Sprintf("%d-%s.%s", jobID, ts, ext))
		os.Rename(path, newPath)
	}
}

// WriteMetaFile writes the job metadata file.
func WriteMetaFile(paths JobPaths, jobID int64, workingDir, command, description string, startTime int64, sourceSHA string) error {
	hostname, _ := os.Hostname()
	var lines []string
	lines = append(lines, fmt.Sprintf("job_id=%d", jobID))
	lines = append(lines, fmt.Sprintf("working_dir=%s", workingDir))
	lines = append(lines, fmt.Sprintf("command=%s", command))
	lines = append(lines, fmt.Sprintf("start_time=%d", startTime))
	lines = append(lines, fmt.Sprintf("host=%s", hostname))
	if description != "" {
		lines = append(lines, fmt.Sprintf("description=%s", description))
	}
	if sourceSHA != "" {
		lines = append(lines, fmt.Sprintf("source_sha256=%s", sourceSHA))
	}
	return os.WriteFile(paths.Meta, []byte(strings.Join(lines, "\n")+"\n"), 0644)
}

// WriteLogHeader writes the initial header block to the log file.
func WriteLogHeader(paths JobPaths, jobID int64, workingDir, command, sourceSHA string) error {
	header := fmt.Sprintf("=== START %s ===\njob_id: %d\ncd: %s\ncmd: %s\n",
		time.Now().Format(time.UnixDate), jobID, workingDir, command)
	if sourceSHA != "" {
		header += fmt.Sprintf("source_sha256: %s\n", sourceSHA)
	}
	header += "===\n"
	return os.WriteFile(paths.Log, []byte(header), 0644)
}

// formatExitSuffix returns the signal/core_dump suffix for exit status strings.
func formatExitSuffix(ei ExitInfo) string {
	var s string
	if ei.Signaled {
		s += fmt.Sprintf(" signal=%s", ei.SignalName())
	}
	if ei.CoreDump {
		s += " core_dump"
	}
	return s
}

// WriteLogFooter appends the end marker to the log file.
func WriteLogFooter(paths JobPaths, ei ExitInfo) error {
	footer := fmt.Sprintf("=== END exit=%d%s %s ===\n", ei.ExitCode, formatExitSuffix(ei), time.Now().Format(time.UnixDate))
	f, err := os.OpenFile(paths.Log, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(footer)
	return err
}

// WriteStatusFile writes the exit code to the status file.
// Format: "{exit_code} [signal={name}] [core_dump]" — first integer is parseable by ReadStatusFile.
func WriteStatusFile(paths JobPaths, ei ExitInfo) error {
	return os.WriteFile(paths.Status, []byte(fmt.Sprintf("%d%s\n", ei.ExitCode, formatExitSuffix(ei))), 0644)
}

// ReadStatusFile reads the exit code from a status file. Returns -1 if not found.
func ReadStatusFile(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return -1, false
	}
	s := strings.TrimSpace(string(data))
	var code int
	if _, err := fmt.Sscanf(s, "%d", &code); err != nil {
		return -1, false
	}
	return code, true
}

// JobCompleted checks if a job has a primary status file (indicating completion).
// Archived status files (from previous runs) are intentionally ignored so that
// requeued jobs are not skipped.
func JobCompleted(logDir string, jobID int64) bool {
	path := filepath.Join(logDir, fmt.Sprintf("%d.status", jobID))
	_, err := os.Stat(path)
	return err == nil
}

// WriteRusageFile writes resource usage data for a completed job.
func WriteRusageFile(paths JobPaths, rs RunningJobState) error {
	var lines []string
	if rs.RusageUserCPU != "" {
		lines = append(lines, "user_cpu_secs="+rs.RusageUserCPU)
	}
	if rs.RusageSysCPU != "" {
		lines = append(lines, "sys_cpu_secs="+rs.RusageSysCPU)
	}
	if rs.RusagePeakRSS > 0 {
		lines = append(lines, fmt.Sprintf("peak_rss_kb=%d", rs.RusagePeakRSS))
	}
	if rs.PeakRSSFromTS > 0 {
		lines = append(lines, fmt.Sprintf("peak_rss_from_ts_kb=%d", rs.PeakRSSFromTS))
	}
	if rs.RusageMaxGPU > 0 {
		lines = append(lines, fmt.Sprintf("max_gpu_mem_mib=%d", rs.RusageMaxGPU))
	}
	if len(rs.GPUDevices) > 0 {
		lines = append(lines, "gpu_devices="+strings.Join(rs.GPUDevices, ","))
	}
	if rs.PeakHostMemRatio > 0 {
		lines = append(lines, fmt.Sprintf("peak_host_mem_ratio=%.4f", rs.PeakHostMemRatio))
	}
	if rs.PeakMemPressure != "" {
		lines = append(lines, "peak_mem_pressure="+string(rs.PeakMemPressure))
	}
	if len(lines) == 0 {
		return nil
	}
	return os.WriteFile(paths.Rusage, []byte(strings.Join(lines, "\n")+"\n"), 0644)
}

// writeReasonFile writes a single-line reason string to a file.
func writeReasonFile(path, reason string) error {
	if reason == "" {
		return nil
	}
	return os.WriteFile(path, []byte(reason+"\n"), 0644)
}

// readReasonFile reads a single-line reason string from a file. Returns empty string if not found.
func readReasonFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// WriteFailureReasonFile writes a failure reason file for a failed job.
func WriteFailureReasonFile(paths JobPaths, reason string) error {
	return writeReasonFile(paths.FailureReason, reason)
}

// ReadFailureReasonFile reads the failure reason from a file. Returns empty string if not found.
func ReadFailureReasonFile(path string) string {
	return readReasonFile(path)
}

// WriteManifestErrorFile records a non-fatal error encountered while updating
// the artifact manifest from --produces declarations. The completion record
// reader surfaces this in user-facing artifact listings so silent manifest
// failures (e.g. disk full, permission denied) become visible.
func WriteManifestErrorFile(paths JobPaths, reason string) error {
	return writeReasonFile(paths.ManifestError, reason)
}

// ReadManifestErrorFile reads the manifest-error file. Returns empty string if not present.
func ReadManifestErrorFile(path string) string {
	return readReasonFile(path)
}

// Kill reason constants written to the kill_reason file before sending signals.
const (
	KillReasonUserKill        = "user_kill"
	KillReasonRunnerShutdown  = "runner_shutdown"
	KillReasonStoppedDetected = "stopped_detected"
	KillReasonOrphan          = "orphan"
)

// WriteKillReasonFile writes the reason a job was killed, before sending the kill signal.
func WriteKillReasonFile(paths JobPaths, reason string) error {
	return writeReasonFile(paths.KillReason, reason)
}

// ReadKillReasonFile reads the kill reason from a file. Returns empty string if not found.
func ReadKillReasonFile(path string) string {
	return readReasonFile(path)
}

// WriteHeartbeat writes the current epoch to the heartbeat file.
func WriteHeartbeat(paths JobPaths, epoch int64) error {
	return os.WriteFile(paths.Heartbeat, []byte(fmt.Sprintf("%d\n", epoch)), 0644)
}

// PhaseTiming records wall-clock timestamps for each execution phase.
type PhaseTiming struct {
	WrapperStart int64       `json:"wrapper_start"`
	SetupStart   int64       `json:"setup_start"`
	SetupEnd     int64       `json:"setup_end"`
	RunStart     int64       `json:"run_start"`
	RunEnd       int64       `json:"run_end"`
	UploadStart  int64       `json:"upload_start,omitempty"`
	UploadEnd    int64       `json:"upload_end,omitempty"`
	CachePre     *CacheProbe `json:"cache_pre,omitempty"`
	CachePost    *CacheProbe `json:"cache_post,omitempty"`
	SetupSeconds *int64      `json:"setup_seconds,omitempty"`
}

// WritePhasesFile writes a phases.json file alongside other job log files.
func WritePhasesFile(paths JobPaths, phases PhaseTiming) error {
	data, err := json.MarshalIndent(phases, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(paths.Phases, data, 0644)
}

// OutputFile describes a single discovered output file.
type OutputFile struct {
	RelPath   string `json:"rel_path"` // relative to workDir (e.g., "output/results.json")
	SizeBytes int64  `json:"size_bytes"`
}

// Upload status constants used by OutputDirUpload, OutputUploadResult, and UploadSummary.
const (
	UploadStatusOK      = "ok"
	UploadStatusFailed  = "failed"
	UploadStatusPartial = "partial"
)

// OutputDirUpload describes the upload outcome for a single output directory or artifact file.
type OutputDirUpload struct {
	Dir        string `json:"dir"`
	Status     string `json:"status"` // UploadStatus*
	Error      string `json:"error,omitempty"`
	FileCount  int    `json:"file_count,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	RetryCount int    `json:"retry_count,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// OutputUploadResult summarizes output directory uploads for a job.
type OutputUploadResult struct {
	Status          string            `json:"status"` // UploadStatus*
	FileCount       int               `json:"file_count,omitempty"`
	Bytes           int64             `json:"bytes,omitempty"`
	RetryCount      int               `json:"retry_count,omitempty"`
	DurationMS      int64             `json:"duration_ms,omitempty"`
	StartedAtUnix   int64             `json:"started_at_unix,omitempty"`
	CompletedAtUnix int64             `json:"completed_at_unix,omitempty"`
	Dirs            []OutputDirUpload `json:"dirs,omitempty"`
}

// UploadSummary describes a bulk upload such as the per-job results directory.
type UploadSummary struct {
	Status          string `json:"status"` // "ok" or "failed"
	FileCount       int    `json:"file_count,omitempty"`
	Bytes           int64  `json:"bytes,omitempty"`
	RetryCount      int    `json:"retry_count,omitempty"`
	DurationMS      int64  `json:"duration_ms,omitempty"`
	StartedAtUnix   int64  `json:"started_at_unix,omitempty"`
	CompletedAtUnix int64  `json:"completed_at_unix,omitempty"`
	Error           string `json:"error,omitempty"`
}

// CompletionRecord is the structured post-mortem record written as .completion.json.
type CompletionRecord struct {
	ExitCode         int                 `json:"exit_code"`
	Signal           string              `json:"signal,omitempty"`
	SignalName       string              `json:"signal_name,omitempty"`
	CoreDump         bool                `json:"core_dump,omitempty"`
	WallTimeSecs     int64               `json:"wall_time_secs"`
	PeakRSSKB        int64               `json:"peak_rss_kb,omitempty"`
	MaxGPUMemMiB     int                 `json:"max_gpu_mem_mib,omitempty"`
	PeakHostMemRatio float64             `json:"peak_host_mem_ratio,omitempty"`
	PeakMemPressure  MemPressureLevel    `json:"peak_mem_pressure,omitempty"`
	KillReason       string              `json:"kill_reason,omitempty"`
	FailureReason    string              `json:"failure_reason,omitempty"`
	LastHeartbeat    int64               `json:"last_heartbeat,omitempty"`
	LastSample       int64               `json:"last_sample,omitempty"`
	StartTime        int64               `json:"start_time,omitempty"`
	EndTime          int64               `json:"end_time"`
	OutputFiles      []OutputFile        `json:"output_files,omitempty"`
	OutputUpload     *OutputUploadResult `json:"output_upload,omitempty"`
	ResultsUpload    *UploadSummary      `json:"results_upload,omitempty"`
}

// InstanceCompletionManifest is the structured payload written to the R2
// completion marker (campaigns/<instanceID>/.complete) when an instance
// self-destructs. It replaces the legacy bare exit-code string ("0") and
// allows the reconciler to verify that job results were actually uploaded.
type InstanceCompletionManifest struct {
	ExitCode        int                    `json:"exit_code"`
	CompletedAtUnix int64                  `json:"completed_at_unix"`
	Jobs            []JobCompletionSummary `json:"jobs"`
}

// JobCompletionSummary is a per-job entry inside an InstanceCompletionManifest.
type JobCompletionSummary struct {
	JobID        int64  `json:"job_id"`
	ExitCode     int    `json:"exit_code"`
	UploadStatus string `json:"upload_status"` // "ok", "partial", "failed", ""
	OutputBytes  int64  `json:"output_bytes,omitempty"`
}

// ParseCompletionMarker parses the R2 completion marker content, handling both
// the legacy bare exit-code format ("0") and the new JSON manifest format.
// Returns nil for legacy markers.
func ParseCompletionMarker(data string) (*InstanceCompletionManifest, error) {
	data = strings.TrimSpace(data)
	if data == "" {
		return nil, nil
	}
	// Legacy format: bare integer exit code
	if len(data) <= 3 && data[0] != '{' {
		return nil, nil
	}
	var m InstanceCompletionManifest
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		return nil, fmt.Errorf("parse completion manifest: %w", err)
	}
	return &m, nil
}

// AllUploadsOK returns true if every job in the manifest has upload_status "ok".
func (m *InstanceCompletionManifest) AllUploadsOK() bool {
	if m == nil || len(m.Jobs) == 0 {
		return false
	}
	for _, j := range m.Jobs {
		if j.UploadStatus != "ok" {
			return false
		}
	}
	return true
}

// WriteCompletionRecord writes a structured completion.json for post-mortem analysis.
func WriteCompletionRecord(paths JobPaths, ei ExitInfo, rs RunningJobState, killReason, failureReason string, startTime, endTime int64, outputFiles []OutputFile) error {
	peakRSS := rs.RusagePeakRSS
	if peakRSS == 0 && rs.PeakRSSFromTS > 0 {
		peakRSS = rs.PeakRSSFromTS
	}
	rec := CompletionRecord{
		ExitCode:         ei.ExitCode,
		CoreDump:         ei.CoreDump,
		WallTimeSecs:     endTime - startTime,
		PeakRSSKB:        peakRSS,
		MaxGPUMemMiB:     rs.RusageMaxGPU,
		PeakHostMemRatio: rs.PeakHostMemRatio,
		PeakMemPressure:  rs.PeakMemPressure,
		KillReason:       killReason,
		FailureReason:    failureReason,
		LastHeartbeat:    rs.LastHeartbeat,
		LastSample:       rs.LastSample,
		StartTime:        startTime,
		EndTime:          endTime,
		OutputFiles:      outputFiles,
	}
	if ei.Signaled {
		rec.Signal = fmt.Sprintf("%d", int(ei.Signal))
		rec.SignalName = ei.SignalName()
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(paths.Completion, data, 0644)
}

// DetectFailureReason examines the system state to determine why a job failed.
// It checks for disk exhaustion, then evaluates exit code, OOM, and GPU OOM signals.
func DetectFailureReason(exitCode int) string {
	// Disk exhaustion can surface through many exit codes/signals.
	if checkDiskFull() {
		return "disk_full"
	}

	// Exit code 137 = SIGKILL (classic OOM killer)
	if exitCode == 137 {
		if checkDmesgOOM() {
			return "oom"
		}
		return "oom" // SIGKILL is almost always OOM
	}

	// Exit code 139 = SIGSEGV
	if exitCode == 139 {
		return "segfault"
	}

	// Check dmesg for OOM regardless of exit code (best-effort, requires permissions)
	if checkDmesgOOM() {
		return "oom"
	}

	// Check nvidia-smi for GPU OOM (only on Linux where nvidia-smi is available)
	if checkGPUOOM() {
		return "gpu_oom"
	}

	if exitCode == 1 {
		return "error"
	}
	return fmt.Sprintf("exit_%d", exitCode)
}

// checkDmesgOOM checks recent dmesg output for OOM killer messages.
func checkDmesgOOM() bool {
	cmd := exec.Command("dmesg", "--time-format=reltime", "--level=err,crit,alert,emerg")
	out, err := cmd.Output()
	if err != nil {
		// Fallback: try without flags (older kernels, macOS won't have dmesg)
		cmd = exec.Command("dmesg")
		out, err = cmd.Output()
		if err != nil {
			return false
		}
	}

	output := string(out)
	return strings.Contains(output, "Out of memory") ||
		strings.Contains(output, "oom-kill") ||
		strings.Contains(output, "Killed process") ||
		strings.Contains(output, "invoked oom-killer")
}

// checkDiskFull checks recent system signals for ENOSPC/disk exhaustion.
// It looks at dmesg and falls back to df usage on "/" for containerized setups.
func checkDiskFull() bool {
	if checkDmesgDiskFull() {
		return true
	}
	return checkRootDiskUsage()
}

func checkDmesgDiskFull() bool {
	cmd := exec.Command("dmesg", "--time-format=reltime", "--level=err,crit,alert,emerg")
	out, err := cmd.Output()
	if err != nil {
		// Fallback: try without flags (older kernels, macOS won't have dmesg)
		cmd = exec.Command("dmesg")
		out, err = cmd.Output()
		if err != nil {
			return false
		}
	}
	output := strings.ToLower(string(out))
	return strings.Contains(output, "no space left on device") ||
		strings.Contains(output, "enospc")
}

func checkRootDiskUsage() bool {
	cmd := exec.Command("df", "-P", "/")
	out, err := cmd.Output()
	if err != nil {
		// Fallback for environments that do not support -P.
		cmd = exec.Command("df", "/")
		out, err = cmd.Output()
		if err != nil {
			return false
		}
	}

	lines := strings.Split(string(out), "\n")
	for i := 1; i < len(lines); i++ {
		fields := strings.Fields(lines[i])
		for _, field := range fields {
			if !strings.HasSuffix(field, "%") {
				continue
			}
			pct, err := strconv.Atoi(strings.TrimSuffix(field, "%"))
			if err != nil {
				continue
			}
			if pct >= 99 {
				return true
			}
			break
		}
	}
	return false
}

// checkGPUOOM checks nvidia-smi for GPU memory errors.
func checkGPUOOM() bool {
	cmd := exec.Command("nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader")
	_, err := cmd.Output()
	if err != nil {
		// nvidia-smi not available or failed — check if the error itself indicates OOM
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := string(exitErr.Stderr)
			if strings.Contains(stderr, "out of memory") || strings.Contains(stderr, "CUDA_ERROR_OUT_OF_MEMORY") {
				return true
			}
		}
		return false
	}
	return false
}

// WriteSample appends a sample line to the samples file.
func WriteSample(paths JobPaths, epoch int64, cpuPct int, gpuMiB *int) error {
	f, err := os.OpenFile(paths.Samples, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	line := fmt.Sprintf("%d %d", epoch, cpuPct)
	if gpuMiB != nil {
		line += fmt.Sprintf(" %d", *gpuMiB)
	}
	_, err = fmt.Fprintln(f, line)
	return err
}

// GetJobGPUDevices extracts GPU device indices from a CommandJob.
// Checks: 1) "gpu" field, 2) CUDA_VISIBLE_DEVICES in env vars.
func GetJobGPUDevices(job *opsqueue.CommandJob) []string {
	if job.GPU != "" {
		return splitCSV(job.GPU)
	}
	for _, ev := range job.Env {
		if strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") {
			val := strings.TrimPrefix(ev, "CUDA_VISIBLE_DEVICES=")
			if val != "" {
				return splitCSV(val)
			}
		}
	}
	return nil
}

// JobHasExplicitGPUIntent reports whether the user declared GPU intent on the
// job via GPUClass, GPU, or GPUMem. Auto-assigned devices from a GPU host do
// not count. Used to gate GPU-specific runtime behavior (e.g. idle watchdog)
// on compute-only jobs scheduled onto GPU rentals.
func JobHasExplicitGPUIntent(job *opsqueue.CommandJob) bool {
	return job.GPUClass != "" || job.GPU != "" || job.GPUMem != nil
}

// GetJobGPUMem returns the GPU memory reservation in GB per device.
func GetJobGPUMem(job *opsqueue.CommandJob, defaultGB int) int {
	if job.GPUMem != nil {
		return *job.GPUMem
	}
	devices := GetJobGPUDevices(job)
	if len(devices) == 0 && job.GPUClass == "" {
		return 0
	}
	return defaultGB
}

// HasTag checks if a job has a specific tag.
func HasTag(job *opsqueue.CommandJob, tag string) bool {
	for _, t := range job.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// WriteTimeseriesSample appends a single JSON line to the timeseries file.
func WriteTimeseriesSample(paths JobPaths, sample TimeseriesSample) error {
	f, err := os.OpenFile(paths.Timeseries, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = f.Write(data)
	return err
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
