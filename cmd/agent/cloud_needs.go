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

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/retry"
	"github.com/osteele/weft/internal/runner"
)

var copyCloudNeedFromR2Func = copyCloudNeedFromR2

func stageCloudPayloads(bucket string, job *cloud.AgentJob) (string, error) {
	if job == nil || len(job.Payloads) == 0 {
		return "", nil
	}
	return artifacts.StagePayloads(job.ID, job.Payloads, func(key, destination string) error {
		return copyCloudNeedFromR2Func(bucket, key, destination)
	})
}

func stageCloudNeeds(bucket string, jobID int64, workDir string, needs []cloud.CloudNeed) error {
	if err := stageArtifactNeeds(bucket, jobID, workDir, needs); err != nil {
		return err
	}
	for _, need := range needs {
		if err := writeCloudNeedSatisfiedMarker(jobID, need.Spec); err != nil {
			return err
		}
	}
	return nil
}

func stageArtifactNeeds(bucket string, jobID int64, workDir string, needs []cloud.CloudNeed) error {
	if len(needs) == 0 {
		return nil
	}
	expandedWorkDir := runner.ExpandTilde(workDir)
	if expandedWorkDir == "" {
		expandedWorkDir = workDir
	}
	if expandedWorkDir == "" {
		return fmt.Errorf("artifact staging: empty working dir")
	}
	expandedWorkDir, err := filepath.Abs(expandedWorkDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(expandedWorkDir, 0o755); err != nil {
		return err
	}
	expandedWorkDir, err = filepath.EvalSymlinks(expandedWorkDir)
	if err != nil {
		return err
	}

	fmt.Printf("Job %d: artifact staging start (%d artifact%s)\n", jobID, len(needs), pluralSuffix(len(needs)))
	oplog.LogJob(oplog.OpJobSync, jobID, "",
		oplog.WithDetailf("artifact staging start (%d artifact%s)", len(needs), pluralSuffix(len(needs))),
	)

	for _, need := range needs {
		label := need.Spec
		if strings.TrimSpace(label) == "" {
			label = need.Path
		}
		fmt.Printf("Job %d: artifact staging attempt: %s\n", jobID, label)
		oplog.LogJob(oplog.OpJobSync, jobID, "", oplog.WithDetailf("artifact staging attempt: %s", label))
		if err := stageOneCloudNeed(bucket, jobID, expandedWorkDir, need, label); err != nil {
			return err
		}

		fmt.Printf("Job %d: artifact staging success: %s\n", jobID, label)
		oplog.LogJob(oplog.OpJobSync, jobID, "", oplog.WithDetailf("artifact staging success: %s", label))
	}

	fmt.Printf("Job %d: artifact staging complete (%d artifact%s)\n", jobID, len(needs), pluralSuffix(len(needs)))
	oplog.LogJob(oplog.OpJobSync, jobID, "",
		oplog.WithDetailf("artifact staging complete (%d artifact%s)", len(needs), pluralSuffix(len(needs))),
	)
	return nil
}

func stageOneCloudNeed(bucket string, jobID int64, expandedWorkDir string, need cloud.CloudNeed, label string) error {
	targetPath, err := archiveMemberTarget(expandedWorkDir, strings.TrimPrefix(need.Path, "/"))
	if err != nil {
		return fmt.Errorf("prepare local staging path for %q: %w", label, err)
	}
	if err := prepareCloudNeedTarget(targetPath, need.ContentType); err != nil {
		return fmt.Errorf("prepare local staging path for %q: %w", label, err)
	}
	copyTarget := targetPath
	if need.ContentType == "directory" {
		copyTarget = targetPath + ".weft-archive.tar.gz"
		defer os.Remove(copyTarget)
	}
	if err := copyCloudNeedFromR2Func(bucket, need.R2Key, copyTarget); err != nil {
		oplog.LogJob(oplog.OpJobSync, jobID, "", oplog.WithDetailf("artifact staging fail: %s", label), oplog.WithError(err))
		return fmt.Errorf("artifact staging failed for %q: %w", label, err)
	}
	if need.ContentType == "directory" {
		if err := extractCloudNeedArchive(copyTarget, targetPath); err != nil {
			return fmt.Errorf("extract cloud directory asset %q: %w", label, err)
		}
	}
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

// root is an absolute, symlink-resolved directory. Archive member names stay
// strictly relative; dependency paths are normalized before reaching this helper.
func archiveMemberTarget(root, name string) (string, error) {
	if strings.ContainsAny(name, "\\\x00") {
		return "", fmt.Errorf("unsafe artifact member path %q", name)
	}
	cleanName := filepath.Clean(filepath.FromSlash(name))
	if cleanName == "." || cleanName == ".." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) || filepath.IsAbs(cleanName) {
		return "", fmt.Errorf("unsafe artifact member path %q", name)
	}
	current := root
	remaining := cleanName
	for remaining != "" {
		part, rest, _ := strings.Cut(remaining, string(filepath.Separator))
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return filepath.Join(current, rest), nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			relative, err := filepath.Rel(root, resolved)
			if err != nil || !filepath.IsLocal(relative) {
				return "", fmt.Errorf("artifact member escapes target directory: %q", name)
			}
			current = resolved
		}
		remaining = rest
	}
	if current == root {
		return "", fmt.Errorf("artifact member resolves to target directory: %q", name)
	}
	return current, nil
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
