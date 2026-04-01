package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// uploadOpslog flushes the oplog and uploads it to R2, then re-initializes for continued use.
func uploadOpslog(bucket string, instanceID int64, logDir string) {
	uploadOpslogMu.Lock()
	defer uploadOpslogMu.Unlock()

	oplogPath := filepath.Join(logDir, agentOpslogFile)
	oplog.Close() // flush

	key := r2keys.InstanceOpslog(instanceID)
	data, err := os.ReadFile(oplogPath)
	if err == nil && len(data) > 0 {
		if err := r2PutReader(bucket, key, strings.NewReader(string(data))); err != nil {
			fmt.Fprintf(os.Stderr, "upload opslog: %v\n", err)
		}
	}

	// Re-open for continued logging
	if err := oplog.Init(oplogPath, 0); err != nil {
		fmt.Fprintf(os.Stderr, "warning: re-init opslog: %v\n", err)
	}
}
