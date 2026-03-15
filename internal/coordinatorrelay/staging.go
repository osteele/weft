package coordinatorrelay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/ssh"
	weftsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/workdir"
)

const stageManifestName = "manifest.json"

type StagedBundle struct {
	Root string
	Ref  *SourceBundleRef
}

type stageManifest struct {
	Hash    string              `json:"hash"`
	Entries []SourceBundleEntry `json:"entries"`
}

// StageSources prepares a snapshot for coordinator-side hydration.
func StageSources(ctx context.Context, cfg *config.Config, r2Client *r2.Client, workingDir string, inputs []string) (*StagedBundle, error) {
	if strings.TrimSpace(workingDir) == "" {
		return nil, nil
	}
	localDir := workdir.ResolveLocal(workingDir)
	if localDir == "" {
		return nil, nil
	}

	stageRoot, entries, err := buildStageRoot(localDir, workingDir, inputs)
	if err != nil {
		return nil, err
	}

	tmpTarball, hash, err := weftsync.CreateSourceTarball(stageRoot)
	if err != nil {
		_ = os.RemoveAll(stageRoot)
		return nil, fmt.Errorf("hash staged bundle: %w", err)
	}
	_ = os.Remove(tmpTarball)

	ref := &SourceBundleRef{
		Kind:    SourceRefCache,
		Hash:    hash,
		Path:    filepath.Join("~/.cache/weft/coordinator-sources", hash),
		Entries: entries,
	}
	if err := writeStageManifest(stageRoot, hash, entries); err != nil {
		_ = os.RemoveAll(stageRoot)
		return nil, err
	}

	coordinatorHost := ""
	if cfg != nil {
		coordinatorHost = cfg.CoordinatorHost
	}
	if coordinatorHost != "" && stageToCoordinatorCache(coordinatorHost, stageRoot, ref.Path) == nil {
		return &StagedBundle{Root: stageRoot, Ref: ref}, nil
	}
	if r2Client == nil {
		_ = os.RemoveAll(stageRoot)
		return nil, fmt.Errorf("R2 is required when coordinator staging over SSH is unavailable")
	}
	key, err := weftsync.UploadSourceToR2(ctx, r2Client, stageRoot)
	if err != nil {
		_ = os.RemoveAll(stageRoot)
		return nil, fmt.Errorf("upload staged bundle to R2: %w", err)
	}
	ref.Kind = SourceRefR2
	ref.Path = key
	return &StagedBundle{Root: stageRoot, Ref: ref}, nil
}

// HydrateSources materializes a staged bundle from local coordinator cache or R2 onto a host.
func HydrateSources(ctx context.Context, r2Client *r2.Client, host string, ref *SourceBundleRef) error {
	if ref == nil || ref.Kind == SourceRefNone || len(ref.Entries) == 0 {
		return nil
	}
	root, cleanup, err := resolveBundleRoot(ctx, r2Client, ref)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	for _, entry := range ref.Entries {
		localPath := filepath.Join(root, entry.RelPath)
		switch entry.Kind {
		case SourceEntryDir:
			if err := weftsync.SyncTree(host, localPath, entry.RemotePath, entry.Delete); err != nil {
				return err
			}
		case SourceEntryFile:
			if err := weftsync.SyncFile(host, localPath, entry.RemotePath); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown source entry kind %q", entry.Kind)
		}
	}
	return nil
}

func buildStageRoot(localDir, workingDir string, inputs []string) (string, []SourceBundleEntry, error) {
	root, err := os.MkdirTemp("", "weft-coordinator-stage-*")
	if err != nil {
		return "", nil, err
	}

	var entries []SourceBundleEntry
	mainTarball, _, err := weftsync.CreateSourceTarball(localDir)
	if err != nil {
		_ = os.RemoveAll(root)
		return "", nil, err
	}
	defer os.Remove(mainTarball)

	mainRel := filepath.Join("entries", "0")
	mainAbs := filepath.Join(root, mainRel)
	if err := os.MkdirAll(mainAbs, 0755); err != nil {
		_ = os.RemoveAll(root)
		return "", nil, err
	}
	if err := weftsync.ExtractTarball(mainTarball, mainAbs); err != nil {
		_ = os.RemoveAll(root)
		return "", nil, err
	}
	entries = append(entries, SourceBundleEntry{
		ID:         "main",
		Kind:       SourceEntryDir,
		RelPath:    mainRel,
		RemotePath: workingDir,
		Delete:     true,
	})

	extraPaths := weftsync.CollectExtraPaths(inputs, localDir)
	for i, extra := range extraPaths {
		abs := extra
		if strings.HasPrefix(abs, "~/") {
			home, _ := os.UserHomeDir()
			abs = filepath.Join(home, abs[2:])
		}
		info, err := os.Stat(abs)
		if err != nil {
			continue
		}
		rel := filepath.Join("entries", fmt.Sprintf("extra-%d", i))
		dst := filepath.Join(root, rel)
		if info.IsDir() {
			if err := weftsync.CopyPath(abs, dst); err != nil {
				_ = os.RemoveAll(root)
				return "", nil, err
			}
			entries = append(entries, SourceBundleEntry{
				ID:         fmt.Sprintf("extra-%d", i),
				Kind:       SourceEntryDir,
				RelPath:    rel,
				RemotePath: extra,
			})
			continue
		}
		if err := os.MkdirAll(dst, 0755); err != nil {
			_ = os.RemoveAll(root)
			return "", nil, err
		}
		target := filepath.Join(dst, filepath.Base(abs))
		if err := weftsync.CopyPath(abs, target); err != nil {
			_ = os.RemoveAll(root)
			return "", nil, err
		}
		entries = append(entries, SourceBundleEntry{
			ID:         fmt.Sprintf("extra-%d", i),
			Kind:       SourceEntryFile,
			RelPath:    filepath.Join(rel, filepath.Base(abs)),
			RemotePath: extra,
		})
	}

	return root, entries, nil
}

func writeStageManifest(root, hash string, entries []SourceBundleEntry) error {
	data, err := json.MarshalIndent(stageManifest{Hash: hash, Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal stage manifest: %w", err)
	}
	return os.WriteFile(filepath.Join(root, stageManifestName), data, 0644)
}

func stageToCoordinatorCache(host, localRoot, remoteRoot string) error {
	if host == "" {
		return fmt.Errorf("coordinator host is required")
	}
	mkdirCmd := fmt.Sprintf("mkdir -p %s", remoteRoot)
	if _, stderr, err := ssh.RunWithTimeout(host, mkdirCmd, 10); err != nil {
		if stderr != "" {
			return fmt.Errorf("%s", stderr)
		}
		return err
	}
	return weftsync.SyncTree(host, localRoot, remoteRoot, true)
}

func resolveBundleRoot(ctx context.Context, r2Client *r2.Client, ref *SourceBundleRef) (string, func(), error) {
	switch ref.Kind {
	case SourceRefCache:
		home, _ := os.UserHomeDir()
		path := strings.Replace(ref.Path, "~", home, 1)
		return path, nil, nil
	case SourceRefR2:
		if r2Client == nil {
			return "", nil, fmt.Errorf("R2 client is required to hydrate %s bundles", ref.Kind)
		}
		data, err := r2Client.GetObject(ctx, ref.Path)
		if err != nil {
			return "", nil, fmt.Errorf("download staged bundle: %w", err)
		}
		root, err := os.MkdirTemp("", "weft-coordinator-bundle-*")
		if err != nil {
			return "", nil, err
		}
		if err := weftsync.ExtractTarballReader(bytes.NewReader(data), root); err != nil {
			_ = os.RemoveAll(root)
			return "", nil, err
		}
		return root, func() { _ = os.RemoveAll(root) }, nil
	default:
		return "", nil, fmt.Errorf("unknown source reference kind %q", ref.Kind)
	}
}
