package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2keys"
)

const (
	// r2Timeout is the maximum time for any single rclone R2 operation.
	r2Timeout = 15 * time.Second

	// agentOpslogFile is the filename for the agent operations log.
	agentOpslogFile = "agent-ops.jsonl"
)

var uploadOpslogMu sync.Mutex

// r2Get reads the content of an R2 key via rclone. Returns ("", nil) if the key doesn't exist.
func r2Get(bucket, key string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r2Timeout)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, "rclone", "cat", fmt.Sprintf("r2:%s/%s", bucket, key))
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		errText := strings.TrimSpace(stderr.String())
		if isR2MissingKey(errText) {
			return "", nil
		}
		oplog.Log(oplog.OpR2Get, oplog.WithDetail(key),
			oplog.WithError(err), oplog.WithDuration(time.Since(start)))
		if errText != "" {
			return "", fmt.Errorf("r2 get %s: %w: %s", key, err, errText)
		}
		return "", fmt.Errorf("r2 get %s: %w", key, err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// r2Put writes content to an R2 key via rclone rcat.
func r2Put(bucket, key, content string) error {
	return r2PutReader(bucket, key, strings.NewReader(content))
}

var r2PutForAgent = r2Put

func r2PutReader(bucket, key string, body io.Reader) error {
	ctx, cancel := context.WithTimeout(context.Background(), r2Timeout)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, "rclone", "rcat", fmt.Sprintf("r2:%s/%s", bucket, key))
	cmd.Stdin = body
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		oplog.Log(oplog.OpR2Put, oplog.WithDetail(key),
			oplog.WithError(err), oplog.WithDuration(time.Since(start)))
		return err
	}
	return nil
}

// r2Delete removes an R2 key via rclone deletefile.
func r2Delete(bucket, key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), r2Timeout)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, "rclone", "deletefile", fmt.Sprintf("r2:%s/%s", bucket, key))
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		oplog.Log(oplog.OpR2Delete, oplog.WithDetail(key),
			oplog.WithError(err), oplog.WithDuration(time.Since(start)))
		return err
	}
	return nil
}

var r2DeleteForAgent = r2Delete

// r2List returns file names under a prefix via rclone lsf. Missing prefixes are empty.
func r2List(bucket, prefix string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r2Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "rclone", "lsf", "--files-only", fmt.Sprintf("r2:%s/%s", bucket, prefix))
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errText := strings.TrimSpace(stderr.String())
		if isR2MissingKey(errText) {
			return nil, nil
		}
		if errText != "" {
			return nil, fmt.Errorf("r2 list %s: %w: %s", prefix, err, errText)
		}
		return nil, fmt.Errorf("r2 list %s: %w", prefix, err)
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var names []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		names = append(names, line)
	}
	sort.Strings(names)
	return names, nil
}

func isR2MissingKey(stderr string) bool {
	if stderr == "" {
		return false
	}
	msg := strings.ToLower(stderr)
	patterns := []string{
		"not found",
		"doesn't exist",
		"does not exist",
		"404",
		"directory not found",
		"object not found",
		"file not found",
	}
	for _, pattern := range patterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}

// uploadOpslog snapshots the current ops log and uploads it to R2.
func uploadOpslog(bucket string, instanceID int64, logDir string) {
	uploadOpslogMu.Lock()
	defer uploadOpslogMu.Unlock()

	oplogPath := filepath.Join(logDir, agentOpslogFile)
	if err := oplog.Sync(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync opslog before upload: %v\n", err)
	}

	key := r2keys.InstanceOpslog(instanceID)
	data, err := os.ReadFile(oplogPath)
	if err == nil && len(data) > 0 {
		if err := r2PutReader(bucket, key, strings.NewReader(string(data))); err != nil {
			fmt.Fprintf(os.Stderr, "upload opslog: %v\n", err)
		}
	}
}

// writeJobAttemptComplete is the single writer of the R2 .complete marker
// (r2keys.JobAttemptComplete, value = exit code). It owns the opslog-before-
// marker ordering: the opslog upload must precede the marker so a
// self-destruct racing post-job work still leaves the agent's diagnostic
// trail in R2 (see runJobSequence). Rewriting the marker with the same exit
// code is idempotent; the background repair path relies on that.
//
// opslogDir is the directory holding agent-ops.jsonl; pass "" only on paths
// whose opslog was already uploaded before the work was enqueued —
// re-uploading from there could overwrite the instance opslog key with
// stale content from an older job's log snapshot.
//
// Uses the r2PutForAgent seam (a var aliasing r2Put) rather than r2Put
// directly so tests can intercept marker writes; the implementations are
// identical.
func writeJobAttemptComplete(bucket string, instanceID int64, opslogDir string, jobID, runID int64, exitCode int) error {
	if bucket == "" {
		return nil
	}
	if opslogDir != "" {
		uploadOpslog(bucket, instanceID, opslogDir)
	}
	return r2PutForAgent(bucket, r2keys.JobAttemptComplete(jobID, runID), fmt.Sprintf("%d", exitCode))
}
