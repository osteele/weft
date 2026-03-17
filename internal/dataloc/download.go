package dataloc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	gosync "sync"

	"github.com/osteele/weft/internal/ssh"
)

const (
	hfCacheLowSpaceFloorBytes = 10 * 1024 * 1024 * 1024
	hfCacheExtraHeadroomBytes = 5 * 1024 * 1024 * 1024
)

var localCommandRunner = defaultLocalCommandRunner

type commandRunnerFunc func(ctx context.Context, host, command string) (string, string, error)

var hostCommandRunner commandRunnerFunc = defaultHostCommandRunner

// DownloadAssetToHost ensures the requested HF asset is present in the host's
// local HF cache, then returns the discovered cache entry.
func DownloadAssetToHost(ctx context.Context, host string, asset DataAsset, revision string) (HostDataEntry, error) {
	if revision == "" {
		revision = "main"
	}
	if err := checkHFCacheFreeSpace(ctx, host, asset); err != nil {
		return HostDataEntry{}, err
	}
	cmd, err := buildHFDownloadCommand(asset, revision)
	if err != nil {
		return HostDataEntry{}, err
	}
	if _, stderr, err := hostCommandRunner(ctx, host, cmd); err != nil {
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

	// Expand PATH to include common user bin dirs so hf/hf_xet are found in
	// non-interactive SSH sessions where ~/.profile may not be sourced.
	// Forward HF token: prefer local coordinator token (so gated models work
	// even if the remote host has no token), fall back to the remote host's
	// cached token file.
	var tokenExport string
	if token := localHFToken(); token != "" {
		tokenExport = "export HF_TOKEN='" + ssh.EscapeForSingleQuotes(token) + "'; "
	} else {
		tokenExport = "if [ -f \"$HOME/.cache/huggingface/token\" ] && [ -z \"${HF_TOKEN:-}\" ]; then " +
			"export HF_TOKEN=$(tr -d '\\n' < \"$HOME/.cache/huggingface/token\"); fi; "
	}
	prefix := "set -e; " +
		"export PATH=\"$HOME/.local/bin:$HOME/bin:${PATH}\"; " +
		tokenExport +
		ResolveHFCacheDirShellVar() + "; mkdir -p \"$_hf_cache\"; "
	body := fmt.Sprintf(
		// _hfdl stores the full download invocation prefix (tool + subcommand
		// where needed). hf_xet and hf require an explicit 'download'
		// subcommand; hf-download is already 'huggingface-cli download' so it
		// takes flags directly.
		"if command -v hf_xet >/dev/null 2>&1; then _hfdl='hf_xet download'; "+
			"elif command -v hf >/dev/null 2>&1; then _hfdl='hf download'; "+
			"elif command -v hf-download >/dev/null 2>&1; then _hfdl=hf-download; "+
			"fi; "+
			"if [ -n \"${_hfdl:-}\" ]; then "+
			"$_hfdl --repo-type %s --revision %s %s >/dev/null; "+
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
	)
	return prefix + body, nil
}

// ResolveHFCacheDirShellVar returns a shell snippet that sets $_hf_cache to the
// HuggingFace hub cache directory, honouring HF_HUB_CACHE > HF_HOME > default,
// matching huggingface_hub precedence rules.
func ResolveHFCacheDirShellVar() string {
	return `if [ -n "${HF_HUB_CACHE:-}" ]; then _hf_cache="$HF_HUB_CACHE"; ` +
		`elif [ -n "${HF_HOME:-}" ]; then _hf_cache="$HF_HOME/hub"; ` +
		`else _hf_cache="$HOME/.cache/huggingface/hub"; fi`
}

// localHFToken returns the HuggingFace token from the local environment,
// checking HF_TOKEN env var then ~/.cache/huggingface/token.
func localHFToken() string {
	if token := os.Getenv("HF_TOKEN"); token != "" {
		return token
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".cache", "huggingface", "token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
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

func defaultHostCommandRunner(ctx context.Context, host string, command string) (string, string, error) {
	if isLocalHost(host) {
		return localCommandRunner(ctx, command)
	}
	return ssh.RunWithContext(ctx, host, command)
}

func defaultLocalCommandRunner(ctx context.Context, command string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-lc", command)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	return stdoutBuf.String(), stderrBuf.String(), err
}

func checkHFCacheFreeSpace(ctx context.Context, host string, asset DataAsset) error {
	freeBytes, err := getHFCacheFreeBytes(ctx, host)
	if err != nil {
		return fmt.Errorf("check HF cache free space on %s: %w", host, err)
	}

	requiredBytes := int64(hfCacheLowSpaceFloorBytes)
	if estimateBytes, err := estimateAssetBytes(asset); err == nil && estimateBytes > 0 {
		estimatedNeed := estimateBytes + estimateBytes/2 + int64(hfCacheExtraHeadroomBytes)
		if estimatedNeed > requiredBytes {
			requiredBytes = estimatedNeed
		}
	}

	if freeBytes < requiredBytes {
		return fmt.Errorf(
			"HF cache volume on %s is low on free space (%s free, need at least %s before downloading %s)",
			host,
			formatByteSize(freeBytes),
			formatByteSize(requiredBytes),
			asset,
		)
	}
	return nil
}

func getHFCacheFreeBytes(ctx context.Context, host string) (int64, error) {
	cmd := ResolveHFCacheDirShellVar() + `; mkdir -p "$_hf_cache" && df -Pk "$_hf_cache" 2>/dev/null | awk 'NR==2 {print $4}'`
	stdout, stderr, err := hostCommandRunner(ctx, host, cmd)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", strings.TrimSpace(stderr), err)
	}
	value := strings.TrimSpace(stdout)
	if value == "" {
		return 0, fmt.Errorf("df returned no free-space value")
	}
	freeKB, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse df output %q: %w", value, err)
	}
	return freeKB * 1024, nil
}

func estimateAssetBytes(asset DataAsset) (int64, error) {
	switch asset.Kind {
	case AssetHFModel:
		return FetchHFModelSize(asset.ID)
	case AssetHFDataset:
		return 0, nil
	default:
		return 0, fmt.Errorf("unsupported asset kind %s", asset.Kind)
	}
}

func formatByteSize(size int64) string {
	if size <= 0 {
		return "0B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	value := float64(size)
	unit := units[0]
	for i := 1; i < len(units) && value >= 1024; i++ {
		value /= 1024
		unit = units[i]
	}
	if unit == "B" {
		return fmt.Sprintf("%d%s", size, unit)
	}
	formatted := fmt.Sprintf("%.1f", value)
	formatted = strings.TrimSuffix(formatted, ".0")
	return formatted + unit
}

var (
	cachedHostname     string
	cachedHostnameOnce gosync.Once
)

func isLocalHost(host string) bool {
	if host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	cachedHostnameOnce.Do(func() {
		cachedHostname, _ = os.Hostname()
	})
	return cachedHostname != "" && host == cachedHostname
}
