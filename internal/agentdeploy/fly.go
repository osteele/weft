package agentdeploy

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/config"
)

// BuildViaFly builds the linux/amd64 agent binary using Fly.io builder settings
// from compatibility WEFT_* environment variables.
func BuildViaFly(version, goos, goarch string, output io.Writer) (string, error) {
	builders, err := resolveBuilders(goos, goarch)
	if err != nil {
		return "", err
	}
	for _, b := range builders {
		if strings.EqualFold(strings.TrimSpace(b.Type), "fly") {
			return BuildViaFlyBuilder(version, goos, goarch, CachePath(version, goos, goarch), b, output)
		}
	}
	return "", fmt.Errorf("fly builder is not configured")
}

// BuildViaFlyBuilder builds linux/amd64 agent binary via a Fly machine and
// downloads it to outputPath.
func BuildViaFlyBuilder(version, goos, goarch, outputPath string, builder config.AgentBuilder, output io.Writer) (string, error) {
	return BuildViaFlyBuilderWithProgress(version, goos, goarch, outputPath, builder, output, nil)
}

// BuildViaFlyBuilderWithProgress is like BuildViaFlyBuilder but also reports
// coarse phase changes (e.g. "starting fly builder", "syncing source",
// "building", "downloading") via onProgress.
func BuildViaFlyBuilderWithProgress(version, goos, goarch, outputPath string, builder config.AgentBuilder, output io.Writer, onProgress BuildProgressFunc) (string, error) {
	if goos != "linux" || goarch != "amd64" {
		return "", fmt.Errorf("Fly builder only supports linux/amd64, got %s/%s", goos, goarch)
	}
	if output == nil {
		output = io.Discard
	}
	if onProgress == nil {
		onProgress = func(string) {}
	}
	root, err := RepoRoot()
	if err != nil {
		return "", fmt.Errorf("locate repo root: %w", err)
	}

	app := strings.TrimSpace(builder.App)
	machine := strings.TrimSpace(builder.Machine)
	if app == "" || machine == "" {
		return "", fmt.Errorf("fly builder requires app and machine")
	}
	baseDir := envOrDefault(builder.BaseDir, "/data/weft-builder")
	goBin := envOrDefault(builder.GoBin, "/usr/local/go/bin/go")
	zigTarget := strings.TrimSpace(builder.ZigTarget)
	zigVersion := envOrDefault(builder.ZigVersion, "0.13.0")

	env := mergeEnvVars(loadRepoEnvVars(root))
	env = maybeSetFlyAccessToken(env)

	remoteWorktree := filepath.Join(baseDir, "worktree")
	remoteOutput := filepath.Join(baseDir, "output", "weft-agent-linux-amd64")
	remoteGoCache := filepath.Join(baseDir, "cache", "go-build")
	remoteGoModCache := filepath.Join(baseDir, "cache", "gomod")

	onProgress("starting fly builder")
	fmt.Fprintf(output, "Starting Fly builder %s/%s...\n", app, machine)
	if err := runCmd(output, "", env, "flyctl", "machine", "start", machine, "-a", app); err != nil {
		return "", fmt.Errorf("start Fly builder: %w", err)
	}
	defer func() {
		onProgress("stopping fly builder")
		fmt.Fprintf(output, "Stopping Fly builder %s/%s...\n", app, machine)
		_ = runCmd(output, "", env, "flyctl", "machine", "stop", machine, "-a", app, "--wait-timeout", "2m")
	}()

	prepare := fmt.Sprintf(`set -euo pipefail
mkdir -p /data/bin /data/lib
NEED_APT=0
if [ -f /data/bin/rsync ]; then
    cp /data/bin/rsync /usr/bin/rsync
    cp /data/lib/libpopt.so.0* /lib/x86_64-linux-gnu/ 2>/dev/null || true
else
    NEED_APT=1
fi
if [ -f /data/bin/xz ]; then
    cp /data/bin/xz /usr/bin/xz
else
    NEED_APT=1
fi
if [ "$NEED_APT" = "1" ]; then
    apt-get update -qq && apt-get install -y --no-install-recommends rsync xz-utils
    cp /usr/bin/rsync /data/bin/rsync
    cp /lib/x86_64-linux-gnu/libpopt.so.0* /data/lib/ || true
    cp /usr/bin/xz /data/bin/xz
fi
mkdir -p %s %s %s %s
`, shellQuote(remoteWorktree), shellQuote(filepath.Join(baseDir, "output")), shellQuote(remoteGoCache), shellQuote(remoteGoModCache))
	onProgress("preparing fly builder")
	if err := runFlySSHConsole(output, env, app, machine, prepare); err != nil {
		return "", fmt.Errorf("prepare Fly builder: %w", err)
	}

	if zigTarget != "" {
		onProgress("installing zig")
		installZig := fmt.Sprintf(`set -euo pipefail
ZIG_VERSION=%s
ZIG_DIR=/data/zig-${ZIG_VERSION}
if [ ! -x "${ZIG_DIR}/zig" ]; then
    mkdir -p /data
    curl -fsSL "https://ziglang.org/download/${ZIG_VERSION}/zig-linux-x86_64-${ZIG_VERSION}.tar.xz" | tar -xJ -C /data
    mv /data/zig-linux-x86_64-${ZIG_VERSION} ${ZIG_DIR}
fi
`, shellQuote(zigVersion))
		if err := runFlySSHConsole(output, env, app, machine, installZig); err != nil {
			return "", fmt.Errorf("install zig on Fly builder: %w", err)
		}
	}

	rshScript, cleanup, err := writeFlyRSHScript(app, machine)
	if err != nil {
		return "", err
	}
	defer cleanup()

	onProgress("syncing source")
	fmt.Fprintln(output, "Syncing source tree to Fly builder...")
	rsyncArgs := []string{
		"-az", "--delete",
		"-e", rshScript,
		"--exclude=.git/", "--exclude=.jj/", "--exclude=.claude/",
		"--exclude=.cache/", "--exclude=.gocache/", "--exclude=.gomodcache/",
		"--exclude=.bench-*-gocache/", "--exclude=.bench-*-gomodcache/",
		"--exclude=testdata/", "--exclude=dist/",
		"--exclude=internal/agentdeploy/binaries/weft-agent-*",
		"--exclude=internal/agentdeploy/binaries/VERSION",
		"--exclude=weft", "--exclude=placement.test",
		root + "/",
		"placeholder:" + remoteWorktree + "/",
	}
	if err := runCmd(output, "", env, "rsync", rsyncArgs...); err != nil {
		return "", fmt.Errorf("sync source to Fly builder: %w", err)
	}

	buildCmd := fmt.Sprintf(`set -euo pipefail
cd %s
GOCACHE=%s GOMODCACHE=%s CGO_ENABLED=1 GOOS=linux GOARCH=amd64`,
		shellQuote(remoteWorktree), shellQuote(remoteGoCache), shellQuote(remoteGoModCache))
	if zigTarget != "" {
		buildCmd += fmt.Sprintf(` CC=%s`, shellQuote(fmt.Sprintf("/data/zig-%s/zig cc -target %s", zigVersion, zigTarget)))
	}
	buildCmd += fmt.Sprintf(` %s build -buildvcs=false -ldflags %s -o %s ./cmd/agent`,
		shellQuote(goBin), shellQuote("-X main.version="+version), shellQuote(remoteOutput))
	onProgress("building")
	if err := runFlySSHConsole(output, env, app, machine, buildCmd); err != nil {
		return "", fmt.Errorf("build on Fly builder: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return "", fmt.Errorf("create output dir: %w", err)
	}
	tmp := outputPath + ".tmp"
	onProgress("downloading")
	fmt.Fprintln(output, "Downloading agent binary from Fly builder...")
	if err := runCmd(output, "", env, "rsync", "-az", "--info=progress2", "-e", rshScript, "placeholder:"+remoteOutput, tmp); err != nil {
		return "", fmt.Errorf("download Fly build output: %w", err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("chmod agent binary: %w", err)
	}
	if err := os.Rename(tmp, outputPath); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("install agent binary: %w", err)
	}
	return outputPath, nil
}

func runFlySSHConsole(output io.Writer, env []string, app, machine, script string) error {
	return runCmd(output, "", env, "flyctl", "ssh", "console", "-a", app, "--machine", machine, "-q", "-C", flyRemoteShellCommand(script))
}

func flyRemoteShellCommand(script string) string {
	return "bash -lc " + shellQuote(script)
}

func runCmd(output io.Writer, dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = output
	cmd.Stderr = output
	return cmd.Run()
}

func writeFlyRSHScript(app, machine string) (path string, cleanup func(), err error) {
	tmpDir, err := os.MkdirTemp("", "weft-fly-rsh-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(tmpDir) }
	scriptPath := filepath.Join(tmpDir, "fly-rsh.sh")
	script := fmt.Sprintf(`#!/bin/bash
while [[ $# -gt 0 && "$1" == -* ]]; do shift; done
shift
exec flyctl ssh console -a %s --machine %s -q -C "$*"
`, shellQuote(app), shellQuote(machine))
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write fly rsh wrapper: %w", err)
	}
	return scriptPath, cleanup, nil
}

func maybeSetFlyAccessToken(env []string) []string {
	if envHasKey(env, "FLY_ACCESS_TOKEN") {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return env
	}
	data, err := os.ReadFile(filepath.Join(home, ".fly", "config.yml"))
	if err != nil {
		return env
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "access_token:") {
			continue
		}
		token := strings.TrimSpace(strings.TrimPrefix(line, "access_token:"))
		if token == "" {
			return env
		}
		return append(env, "FLY_ACCESS_TOKEN="+token)
	}
	return env
}

func envHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}
