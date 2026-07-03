package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
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
		if err := prepareCloudNeedTarget(targetPath, need.ContentType); err != nil {
			return fmt.Errorf("prepare local staging path for %q: %w", label, err)
		}
		copyTarget := targetPath
		if need.ContentType == "directory" {
			copyTarget = targetPath + ".weft-archive.tar.gz"
			defer os.Remove(copyTarget)
		}
		if err := copyCloudNeedFromR2Func(bucket, need.R2Key, copyTarget); err != nil {
			oplog.LogJob(oplog.OpJobSync, jobID, "", oplog.WithDetailf("cloud artifact staging fail: %s", label), oplog.WithError(err))
			return fmt.Errorf("cloud artifact staging failed for %q: %w", label, err)
		}
		if need.ContentType == "directory" {
			if err := extractCloudNeedArchive(copyTarget, targetPath); err != nil {
				return fmt.Errorf("extract cloud directory asset %q: %w", label, err)
			}
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

func prepareCloudNeedTarget(targetPath, contentType string) error {
	if contentType == "directory" {
		return os.MkdirAll(targetPath, 0o755)
	}
	return os.MkdirAll(filepath.Dir(targetPath), 0o755)
}

func extractCloudNeedArchive(archivePath, targetDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	cleanRoot, err := filepath.Abs(targetDir)
	if err != nil {
		return err
	}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		target, err := archiveMemberTarget(cleanRoot, header.Name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, tr)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
}

func archiveMemberTarget(root, name string) (string, error) {
	cleanName := filepath.Clean(filepath.FromSlash(name))
	if cleanName == "." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) || filepath.IsAbs(cleanName) {
		return "", fmt.Errorf("unsafe archive member path %q", name)
	}
	target := filepath.Join(root, cleanName)
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if absTarget != root && !strings.HasPrefix(absTarget, root+string(filepath.Separator)) {
		return "", fmt.Errorf("archive member escapes target directory: %q", name)
	}
	return absTarget, nil
}

func writeCloudNeedSatisfiedMarker(jobID int64, spec string) error {
	parsed, err := runner.ParseNeedsSpec(spec)
	if err != nil {
		if asset, ok := dataloc.ParseAssetRef(spec); ok && (asset.Kind == dataloc.AssetCheckpoint || asset.Kind == dataloc.AssetCorpus) {
			return nil
		}
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
