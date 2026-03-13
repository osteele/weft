package dataloc

import (
	"context"
	"fmt"
	"strconv"

	"github.com/osteele/weft/internal/ssh"
)

// DownloadAssetToHost ensures the requested HF asset is present in the host's
// local HF cache, then returns the discovered cache entry.
func DownloadAssetToHost(ctx context.Context, host string, asset DataAsset, revision string) (HostDataEntry, error) {
	if revision == "" {
		revision = "main"
	}
	cmd, err := buildHFDownloadCommand(asset, revision)
	if err != nil {
		return HostDataEntry{}, err
	}
	if _, stderr, err := ssh.RunWithContext(ctx, host, cmd); err != nil {
		return HostDataEntry{}, fmt.Errorf("download %s on %s: %s: %w", asset, host, stderr, err)
	}

	entries, err := ScanHFCacheDetailed(host)
	if err != nil {
		return HostDataEntry{}, err
	}
	for _, entry := range entries {
		if entry.Asset == asset {
			return entry, nil
		}
	}

	return HostDataEntry{}, fmt.Errorf("asset %s downloaded on %s but not found in HF cache scan", asset, host)
}

func buildHFDownloadCommand(asset DataAsset, revision string) (string, error) {
	repoType, err := hfRepoType(asset.Kind)
	if err != nil {
		return "", err
	}

	repoIDQuoted := "'" + ssh.EscapeForSingleQuotes(asset.ID) + "'"
	revisionQuoted := "'" + ssh.EscapeForSingleQuotes(revision) + "'"
	repoTypePython := strconv.Quote(repoType)
	repoIDPython := strconv.Quote(asset.ID)
	revisionPython := strconv.Quote(revision)

	return fmt.Sprintf(
		"set -e; "+
			"if command -v hf_xet >/dev/null 2>&1; then _hfdl=hf_xet; "+
			"elif command -v hf >/dev/null 2>&1; then _hfdl=hf; "+
			"fi; "+
			"if [ -n \"${_hfdl:-}\" ]; then "+
			"$_hfdl download --repo-type %s --revision %s %s >/dev/null; "+
			"elif python3 -c 'import huggingface_hub' >/dev/null 2>&1; then "+
			"python3 -c \"from huggingface_hub import snapshot_download; snapshot_download(repo_id=%s, repo_type=%s, revision=%s)\" >/dev/null; "+
			"else "+
			"echo 'huggingface_hub not available on remote host (need hf_xet, hf CLI, or python3 package huggingface_hub)' >&2; "+
			"exit 127; "+
			"fi",
		repoType,
		revisionQuoted,
		repoIDQuoted,
		repoIDPython,
		repoTypePython,
		revisionPython,
	), nil
}

func hfRepoType(kind AssetKind) (string, error) {
	switch kind {
	case AssetHFModel:
		return "model", nil
	case AssetHFDataset:
		return "dataset", nil
	default:
		return "", fmt.Errorf("downloads only support hf models and datasets, got %s", kind)
	}
}
