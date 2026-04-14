package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/runner"
)

var copyCloudNeedFromR2Func = copyCloudNeedFromR2

func stageCloudNeeds(bucket string, jobID int64, workDir string, needs []cloud.CloudNeed) error {
	if len(needs) == 0 {
		return nil
	}
	expandedWorkDir := runner.ExpandTilde(workDir)
	if expandedWorkDir == "" {
		expandedWorkDir = workDir
	}
	if expandedWorkDir == "" {
		return fmt.Errorf("cloud artifact staging: empty working dir")
	}

	fmt.Printf("Job %d: cloud artifact staging start (%d artifact%s)\n", jobID, len(needs), pluralSuffix(len(needs)))
	oplog.LogJob(oplog.OpJobSync, jobID, "",
		oplog.WithDetailf("cloud artifact staging start (%d artifact%s)", len(needs), pluralSuffix(len(needs))),
	)

	for _, need := range needs {
		label := need.Spec
		if strings.TrimSpace(label) == "" {
			label = need.Path
		}
		fmt.Printf("Job %d: cloud artifact staging attempt: %s\n", jobID, label)
		oplog.LogJob(oplog.OpJobSync, jobID, "", oplog.WithDetailf("cloud artifact staging attempt: %s", label))

		targetPath := filepath.Join(expandedWorkDir, filepath.FromSlash(strings.TrimPrefix(need.Path, "/")))
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return fmt.Errorf("prepare local staging path for %q: %w", label, err)
		}
		if err := copyCloudNeedFromR2Func(bucket, need.R2Key, targetPath); err != nil {
			oplog.LogJob(oplog.OpJobSync, jobID, "", oplog.WithDetailf("cloud artifact staging fail: %s", label), oplog.WithError(err))
			return fmt.Errorf("cloud artifact staging failed for %q: %w", label, err)
		}

		fmt.Printf("Job %d: cloud artifact staging success: %s\n", jobID, label)
		oplog.LogJob(oplog.OpJobSync, jobID, "", oplog.WithDetailf("cloud artifact staging success: %s", label))
	}

	fmt.Printf("Job %d: cloud artifact staging complete (%d artifact%s)\n", jobID, len(needs), pluralSuffix(len(needs)))
	oplog.LogJob(oplog.OpJobSync, jobID, "",
		oplog.WithDetailf("cloud artifact staging complete (%d artifact%s)", len(needs), pluralSuffix(len(needs))),
	)
	return nil
}

func copyCloudNeedFromR2(bucket, r2Key, targetPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rclone", "copyto", fmt.Sprintf("r2:%s/%s", bucket, r2Key), targetPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("copy r2:%s/%s to %s: %w", bucket, r2Key, targetPath, err)
	}
	return nil
}

func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
