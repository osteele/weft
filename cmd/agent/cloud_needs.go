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
	"github.com/osteele/weft/internal/retry"
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
		if err := writeCloudNeedSatisfiedMarker(jobID, need.Spec); err != nil {
			return err
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

func writeCloudNeedSatisfiedMarker(jobID int64, spec string) error {
	parsed, err := runner.ParseNeedsSpec(spec)
	if err != nil {
		return fmt.Errorf("parse cloud need marker %q: %w", spec, err)
	}
	homeDir, _ := os.UserHomeDir()
	if homeDir == "" {
		homeDir = "/tmp"
	}
	logDir := filepath.Join(homeDir, ".cache", "weft", "logs")
	var marker string
	if parsed.IsAsset() {
		marker = runner.NamedAssetSatisfiedFile(logDir, parsed.AssetName)
	} else {
		marker = runner.ArtifactSatisfiedFile(logDir, parsed.Path, parsed.Version)
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		return fmt.Errorf("create cloud need marker dir for job %d: %w", jobID, err)
	}
	if err := os.WriteFile(marker, []byte("0\n"), 0o644); err != nil {
		return fmt.Errorf("write cloud need marker for job %d: %w", jobID, err)
	}
	return nil
}

func copyCloudNeedFromR2(bucket, r2Key, targetPath string) error {
	const copyTimeout = 20 * time.Minute
	src := fmt.Sprintf("r2:%s/%s", bucket, r2Key)
	return retry.Do(
		context.Background(),
		retry.ExplicitDelays(5*time.Second, 15*time.Second),
		func() error {
			ctx, cancel := context.WithTimeout(context.Background(), copyTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, "rclone", "copyto", src, targetPath)
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("copy %s to %s: %w", src, targetPath, err)
			}
			return nil
		},
	)
}

func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
