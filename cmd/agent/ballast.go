package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/runner"
)

const (
	ballastSizeBytes        = 128 * 1024 * 1024
	diskFullTriggerBytes    = 200 * 1024 * 1024
	diskMonitorPollInterval = 30 * time.Second
)

type diskFailureReport struct {
	TimestampUnix        int64             `json:"timestamp_unix"`
	Phase                string            `json:"phase"`
	JobID                int64             `json:"job_id,omitempty"`
	FilesystemPath       string            `json:"filesystem_path"`
	BallastPath          string            `json:"ballast_path"`
	BallastSizeBytes     int64             `json:"ballast_size_bytes"`
	FreeBytesBefore      int64             `json:"free_bytes_before"`
	FreeBytesAfterDelete int64             `json:"free_bytes_after_delete"`
	DFHuman              string            `json:"df_h,omitempty"`
	DirectoryUsage       map[string]string `json:"directory_usage,omitempty"`
	HFCacheModels        []hfCacheEntry    `json:"hf_cache_models,omitempty"`
}

// hfCacheEntry records a single model or dataset found in the HuggingFace cache.
type hfCacheEntry struct {
	AssetID   string `json:"asset_id"` // e.g. "hf:EleutherAI/pythia-1.4b"
	SizeBytes int64  `json:"size_bytes"`
}

func startDiskMonitor(r2Bucket string, instanceID int64, diskPath, logDir, phaseKey, selfDestructCmd string, getPhase func() string, setPhase func(string)) func() {
	ballastDir := chooseBallastDir(diskPath, logDir)
	ballastPath := filepath.Join(ballastDir, ".weft-ballast")
	if err := ensureBallastFile(ballastPath, ballastSizeBytes); err != nil {
		fmt.Fprintf(os.Stderr, "warning: create ballast file: %v\n", err)
		return func() {}
	}

	var once sync.Once
	done := make(chan struct{})
	stopped := make(chan struct{})
	stop := func() {
		once.Do(func() { close(done) })
		<-stopped
	}

	go func() {
		defer close(stopped)
		defer os.Remove(ballastPath)

		ticker := time.NewTicker(diskMonitorPollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				freeBytes, _, err := probeFilesystem(ballastDir)
				if err != nil || freeBytes > diskFullTriggerBytes {
					continue
				}
				handleDiskFull(r2Bucket, instanceID, ballastDir, ballastPath, logDir, phaseKey, selfDestructCmd, getPhase, setPhase, freeBytes)
				return
			}
		}
	}()

	return stop
}

func chooseBallastDir(diskPath, logDir string) string {
	candidates := []string{diskPath, filepath.Dir(diskPath), logDir}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		info, err := os.Stat(candidate)
		if err == nil && info.IsDir() {
			return candidate
		}
	}
	return "/tmp"
}

func ensureBallastFile(path string, sizeBytes int64) error {
	if sizeBytes <= 0 {
		return nil
	}
	if info, err := os.Stat(path); err == nil && info.Size() == sizeBytes {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, 1024*1024)
	remaining := sizeBytes
	for remaining > 0 {
		n := int64(len(buf))
		if remaining < n {
			n = remaining
		}
		if _, err := f.Write(buf[:n]); err != nil {
			return err
		}
		remaining -= n
	}
	return f.Sync()
}

func probeFilesystem(path string) (freeBytes, totalBytes int64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	bsize := int64(stat.Bsize)
	return int64(stat.Bavail) * bsize, int64(stat.Blocks) * bsize, nil
}

func handleDiskFull(r2Bucket string, instanceID int64, ballastDir, ballastPath, logDir, phaseKey, selfDestructCmd string, getPhase func() string, setPhase func(string), freeBytesBefore int64) {
	phase := getPhase()
	jobID := currentJobIDFromPhase(phase)
	failurePhase := "disk-full"
	if jobID > 0 {
		failurePhase = fmt.Sprintf("disk-full:%d", jobID)
	}
	if setPhase != nil {
		setPhase(failurePhase)
	}
	writePhase(r2Bucket, phaseKey, failurePhase)

	if err := os.Remove(ballastPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "warning: remove ballast file: %v\n", err)
	}
	freeBytesAfter, _, _ := probeFilesystem(ballastDir)

	if jobID > 0 {
		logPath := filepath.Join(logDir, fmt.Sprintf("%d.log", jobID))
		uploadLiveLog(r2Bucket, jobID, 0, logPath, &liveLogUploadState{partHashes: make(map[int]uint64)})
	}
	uploadOpslog(r2Bucket, instanceID, logDir)

	report := collectDiskFailureReport(ballastDir, ballastPath, phase, jobID, freeBytesBefore, freeBytesAfter)
	if data, err := json.MarshalIndent(report, "", "  "); err == nil {
		_ = r2Put(r2Bucket, r2keys.InstanceDiskFailure(instanceID), string(data))
	}

	oplog.Log(oplog.OpPhaseTransition, oplog.WithDetail(failurePhase))
	fmt.Fprintf(os.Stderr, "disk monitor: free space dropped below threshold (%s free); terminating instance\n", formatBytes(freeBytesBefore))
	terminateInstanceForFailure(r2Bucket, instanceID, selfDestructCmd, failurePhase, jobID)
}

func currentJobIDFromPhase(phase string) int64 {
	_, jobIDStr, ok := strings.Cut(phase, ":")
	if !ok {
		return 0
	}
	jobID, err := strconv.ParseInt(jobIDStr, 10, 64)
	if err != nil {
		return 0
	}
	return jobID
}

func collectDiskFailureReport(ballastDir, ballastPath, phase string, jobID int64, freeBytesBefore, freeBytesAfter int64) diskFailureReport {
	report := diskFailureReport{
		TimestampUnix:        time.Now().Unix(),
		Phase:                phase,
		JobID:                jobID,
		FilesystemPath:       ballastDir,
		BallastPath:          ballastPath,
		BallastSizeBytes:     ballastSizeBytes,
		FreeBytesBefore:      freeBytesBefore,
		FreeBytesAfterDelete: freeBytesAfter,
		DirectoryUsage:       make(map[string]string),
	}
	if out, err := commandOutput(5*time.Second, "df", "-h", ballastDir); err == nil {
		report.DFHuman = out
	}
	home, _ := os.UserHomeDir()
	for label, path := range map[string]string{
		"root_path":   ballastDir,
		"uv_cache":    filepath.Join(home, ".cache", "uv"),
		"huggingface": filepath.Join(home, ".cache", "huggingface"),
		"weft_cache":  filepath.Join(home, ".cache", "weft"),
	} {
		if out, err := commandOutput(10*time.Second, "du", "-sh", path); err == nil {
			report.DirectoryUsage[label] = out
		}
	}
	report.HFCacheModels = scanHFCache(filepath.Join(home, ".cache", "huggingface", "hub"))
	return report
}

// scanHFCache lists HuggingFace model/dataset directories and their sizes.
// Uses dataloc.ParseHFDirName for directory name parsing to stay consistent
// with the remote HF cache scanner.
func scanHFCache(hubDir string) []hfCacheEntry {
	entries, err := os.ReadDir(hubDir)
	if err != nil {
		return nil
	}
	var result []hfCacheEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		asset, ok := dataloc.ParseHFDirName(entry.Name())
		if !ok {
			continue
		}
		dirPath := filepath.Join(hubDir, entry.Name())
		result = append(result, hfCacheEntry{
			AssetID:   asset.Ref(),
			SizeBytes: runner.DirSizeBytes(dirPath),
		})
	}
	return result
}

func commandOutput(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func terminateInstanceForFailure(bucket string, instanceID int64, selfDestructCmd, phase string, jobID int64) {
	marker := &instanceintent.Marker{
		TerminalStatus:    db.LaunchStatusFailed,
		TerminationReason: db.TerminationReasonDiskFull,
		Phase:             phase,
		JobID:             jobID,
		RequestedAtUnix:   time.Now().Unix(),
	}
	writeTerminationIntent(bucket, instanceID, *marker)
	executeSelfDestruct(bucket, instanceID, selfDestructCmd, marker)
}

func formatBytes(n int64) string {
	if n <= 0 {
		return "0B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for value := n / unit; value >= unit; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
