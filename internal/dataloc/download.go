package dataloc

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	gosync "sync"
	"time"

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
// local HF cache, then returns the discovered cache entry. The download itself
// runs as a detached process on the remote (see runDetachedRemoteCommand) so a
// VPN blip or SSH ServerAliveCountMax tripping mid-download doesn't kill the
// transfer — only the cheap poll calls need stable connectivity.
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
	exitCode, stderr, err := runDetachedRemoteCommand(ctx, host, cmd)
	if err != nil {
		return HostDataEntry{}, formatHFDownloadError(host, asset, stderr, err)
	}
	if exitCode != 0 {
		return HostDataEntry{}, formatHFDownloadError(host, asset, stderr, fmt.Errorf("remote download exited %d", exitCode))
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

// detachedPollInterval is how long runDetachedRemoteCommand waits between
// successive polls of the remote .status file. Each poll is a short SSH call
// (typically <1s); the interval bounds the wall-clock noise added on top of
// the actual remote work.
var detachedPollInterval = 5 * time.Second

// detachedSpawnTimeout caps the SSH call that spawns the detached process.
// Spawning is cheap (a few file writes and a fork) so a short bound here just
// fails fast on connectivity issues rather than blocking on a hung session.
var detachedSpawnTimeout = 30 * time.Second

// runDetachedRemoteCommand runs `command` on `host` as a nohup'd detached
// process, polling a small status file for completion. Each SSH call (spawn
// + each poll + final reap) is short and well within the ServerAliveCountMax
// window of the SSH connection pool, so VPN blips or network jitter that
// would kill a multi-minute streaming SSH command are tolerated here — the
// remote work continues running, and the next poll picks up the status file
// when it lands.
//
// Returns the remote command's exit code and any captured stderr. On context
// cancel, sends SIGTERM to the remote process (best-effort) before returning
// ctx.Err().
//
// The remote uses ~/.cache/weft/data-fetch/<runID>/ as a per-call scratch
// directory holding pid, status, and stderr.log. The directory is left in
// place after success for ad-hoc inspection; callers that want to clean up
// can do so themselves.
func runDetachedRemoteCommand(ctx context.Context, host, command string) (int, string, error) {
	runID, err := newRunID()
	if err != nil {
		return -1, "", fmt.Errorf("generate run id: %w", err)
	}
	// Note: $HOME is expanded by the remote shell, not Go. We avoid embedding
	// the resolved absolute path here so the same script works regardless of
	// the remote user's home.
	runDir := fmt.Sprintf("$HOME/.cache/weft/data-fetch/%s", runID)

	// Spawn the command in a nohup'd background bash with an EXIT trap that
	// writes the exit code to the status file. Heredoc keeps the user
	// command literal — no Go-side escaping of $vars, backticks, etc. The
	// heredoc delimiter is unique enough not to collide with the body.
	const heredocDelim = "WEFT_DETACHED_CMD_END"
	if strings.Contains(command, heredocDelim) {
		return -1, "", fmt.Errorf("command contains reserved heredoc delimiter %q", heredocDelim)
	}
	spawnCmd := fmt.Sprintf(`set -e
D=%s
mkdir -p "$D"
cat > "$D/cmd.sh" <<'%s'
%s
%s
chmod +x "$D/cmd.sh"
nohup bash -c "trap '_e=\$?; echo \$_e > \"$D/status\"; exit \$_e' EXIT TERM INT; bash \"$D/cmd.sh\"" > "$D/stdout.log" 2> "$D/stderr.log" < /dev/null &
echo $! > "$D/pid"
disown 2>/dev/null || true
echo OK
`, runDir, heredocDelim, command, heredocDelim)

	spawnCtx, spawnCancel := context.WithTimeout(ctx, detachedSpawnTimeout)
	_, spawnStderr, err := hostCommandRunner(spawnCtx, host, spawnCmd)
	spawnCancel()
	if err != nil {
		return -1, normalizeShellStderr(spawnStderr), fmt.Errorf("spawn detached command on %s: %w", host, err)
	}

	checkCmd := fmt.Sprintf(`D=%s
if [ -f "$D/status" ]; then
  echo "STATUS=$(cat "$D/status")"
  echo "---STDERR---"
  cat "$D/stderr.log" 2>/dev/null || true
elif [ -f "$D/pid" ] && kill -0 $(cat "$D/pid") 2>/dev/null; then
  echo "RUNNING"
else
  echo "ORPHANED"
  echo "---STDERR---"
  cat "$D/stderr.log" 2>/dev/null || true
fi
`, runDir)

	killCmd := fmt.Sprintf(`D=%s
if [ -f "$D/pid" ]; then kill -TERM $(cat "$D/pid") 2>/dev/null || true; fi
`, runDir)

	slog.Debug("detached remote command spawned", "host", host, "run_id", runID)

	timer := time.NewTimer(detachedPollInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			// Best-effort kill using a fresh context so a timed-out parent
			// doesn't prevent us from sending the SIGTERM.
			killCtx, killCancel := context.WithTimeout(context.Background(), detachedSpawnTimeout)
			_, _, _ = hostCommandRunner(killCtx, host, killCmd)
			killCancel()
			return -1, "", ctx.Err()
		case <-timer.C:
		}
		timer.Reset(detachedPollInterval)

		pollCtx, pollCancel := context.WithTimeout(ctx, detachedSpawnTimeout)
		stdout, _, pollErr := hostCommandRunner(pollCtx, host, checkCmd)
		pollCancel()
		if pollErr != nil {
			slog.Debug("detached poll SSH failed; will retry", "host", host, "run_id", runID, "error", pollErr)
			continue
		}

		head, capturedStderr := splitDetachedPoll(stdout)
		switch {
		case strings.HasPrefix(head, "STATUS="):
			ecStr := strings.TrimSpace(strings.TrimPrefix(head, "STATUS="))
			ec, perr := strconv.Atoi(ecStr)
			if perr != nil {
				return -1, capturedStderr, fmt.Errorf("parse remote status %q: %w", ecStr, perr)
			}
			return ec, capturedStderr, nil
		case head == "RUNNING":
			continue
		case head == "ORPHANED":
			return -1, capturedStderr, fmt.Errorf("remote process orphaned (pid not alive, no status file)")
		default:
			slog.Debug("unexpected detached poll output", "host", host, "run_id", runID, "head", head)
		}
	}
}

// splitDetachedPoll separates the STATUS/RUNNING/ORPHANED header from the
// stderr tail in the format produced by the checkCmd shell snippet.
func splitDetachedPoll(out string) (head, stderr string) {
	parts := strings.SplitN(out, "---STDERR---\n", 2)
	head = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		stderr = parts[1]
	}
	return head, stderr
}

// newRunID returns a short, mostly-unique identifier for a single detached
// remote command invocation. Format: <unix-seconds>-<8 random hex bytes>.
func newRunID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d-%x", time.Now().Unix(), b), nil
}

func formatHFDownloadError(host string, asset DataAsset, stderr string, runErr error) error {
	stderr = normalizeShellStderr(stderr)
	repoType, _ := hfRepoType(asset.Kind)
	lowerStderr := strings.ToLower(stderr)
	if strings.Contains(lowerStderr, "repository not found") {
		return fmt.Errorf(
			"download %s on %s: Hugging Face %s repo %q not found; update the job input (expected hf:<model> or hf-dataset:<dataset>) or authenticate with HF_TOKEN for private repos: %w",
			asset,
			host,
			repoType,
			asset.ID,
			runErr,
		)
	}
	if strings.Contains(lowerStderr, "no local file found") && strings.Contains(lowerStderr, "retrying") {
		return fmt.Errorf(
			"download %s on %s failed while Hugging Face retried missing files; this is usually an auth, repo-type, revision, or dataset shard issue. Last output: %s: %w",
			asset,
			host,
			stderr,
			runErr,
		)
	}
	if stderr == "" {
		return fmt.Errorf("download %s on %s failed: %w", asset, host, runErr)
	}
	return fmt.Errorf("download %s on %s failed: %s: %w", asset, host, stderr, runErr)
}

func normalizeShellStderr(stderr string) string {
	parts := strings.Fields(stderr)
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
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
