package agentdeploy

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/osteele/weft/internal/config"
)

var (
	prewarmMu       sync.Mutex
	prewarmInFlight = map[string]struct{}{}
)

func targetKey(version, goos, goarch string) string {
	return version + ":" + goos + ":" + goarch
}

// StartBackgroundPrewarm triggers a best-effort background build for the
// requested target. It returns immediately and never reports errors to callers.
func StartBackgroundPrewarm(goos, goarch string) {
	version, err := LocalAgentVersion()
	if err != nil {
		slog.Debug("skip agent prewarm: local version unavailable", "component", "agentdeploy", "error", err)
		return
	}
	if _, err := os.Stat(CachePath(version, goos, goarch)); err == nil {
		return
	}

	key := targetKey(version, goos, goarch)
	prewarmMu.Lock()
	if _, ok := prewarmInFlight[key]; ok {
		prewarmMu.Unlock()
		return
	}
	prewarmInFlight[key] = struct{}{}
	prewarmMu.Unlock()

	go func() {
		defer func() {
			prewarmMu.Lock()
			delete(prewarmInFlight, key)
			prewarmMu.Unlock()
		}()
		if _, err := EnsureBuilt(version, goos, goarch); err != nil {
			slog.Debug("background agent prewarm failed",
				"component", "agentdeploy",
				"goos", goos,
				"goarch", goarch,
				"error", err)
			return
		}
		slog.Debug("background agent prewarm complete",
			"component", "agentdeploy",
			"goos", goos,
			"goarch", goarch,
			"version", version)
	}()
}

func buildOnDemand(version, goos, goarch, outputPath string, output io.Writer) error {
	builders, err := resolveBuilders(goos, goarch)
	if err != nil {
		return err
	}
	if len(builders) == 0 {
		return fmt.Errorf("%w for %s/%s: no builders configured in ~/.config/weft/config.toml [agent_build] and no WEFT_* fallback vars found",
			ErrAgentNotAvailable, goos, goarch)
	}

	var attempts []string
	for _, builder := range builders {
		err := runBuilder(version, goos, goarch, outputPath, builder, output)
		if err == nil {
			return nil
		}
		label := builder.Type
		if builder.Name != "" {
			label = builder.Name
		} else if builder.Host != "" {
			label = builder.Type + ":" + builder.Host
		} else if builder.App != "" && builder.Machine != "" {
			label = builder.Type + ":" + builder.App + "/" + builder.Machine
		}
		attempts = append(attempts, fmt.Sprintf("%s: %v", label, err))
	}
	return fmt.Errorf("%w for %s/%s: all builders failed (%s)",
		ErrAgentNotAvailable, goos, goarch, strings.Join(attempts, "; "))
}

func runBuilder(version, goos, goarch, outputPath string, builder config.AgentBuilder, output io.Writer) error {
	switch strings.ToLower(strings.TrimSpace(builder.Type)) {
	case "ssh":
		return buildViaSSHBuilder(version, goos, goarch, outputPath, builder)
	case "fly":
		var captured bytes.Buffer
		_, err := BuildViaFlyBuilder(version, goos, goarch, outputPath, builder, io.MultiWriter(&captured, output))
		if err == nil {
			return nil
		}
		details := strings.TrimSpace(captured.String())
		if details == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, trimErrorOutput(details))
	default:
		return fmt.Errorf("unknown builder type %q", builder.Type)
	}
}

func trimErrorOutput(s string) string {
	const maxChars = 1200
	if len(s) <= maxChars {
		return s
	}
	return "..." + s[len(s)-maxChars:]
}

func resolveBuilders(goos, goarch string) ([]config.AgentBuilder, error) {
	if goos != "linux" || goarch != "amd64" {
		return nil, fmt.Errorf("%w for %s/%s: on-demand builders are currently only supported for linux/amd64",
			ErrAgentNotAvailable, goos, goarch)
	}

	cfg, err := config.Load()
	if err == nil && cfg != nil && len(cfg.AgentBuild.LinuxAMD64Builders) > 0 {
		return normalizeBuilders(cfg.AgentBuild.LinuxAMD64Builders), nil
	}

	// Compatibility fallback: existing WEFT_* environment variables.
	root, _ := RepoRoot()
	repoEnv := map[string]string{}
	if root != "" {
		repoEnv = loadRepoEnvVars(root)
	}
	env := func(key string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return strings.TrimSpace(repoEnv[key])
	}

	var out []config.AgentBuilder
	if host := env("WEFT_LINUX_BUILDER_HOST"); host != "" {
		out = append(out, config.AgentBuilder{
			Type:      "ssh",
			Name:      "linux-ssh-builder",
			Host:      host,
			RemoteDir: envOrDefault(env("WEFT_LINUX_BUILDER_DIR"), "~/.cache/weft/agent-build"),
			GoBin:     envOrDefault(env("WEFT_LINUX_BUILDER_GO"), "/usr/local/go/bin/go"),
		})
	}
	if app := env("WEFT_FLY_BUILDER_APP"); app != "" && env("WEFT_FLY_BUILDER_MACHINE") != "" {
		out = append(out, config.AgentBuilder{
			Type:       "fly",
			Name:       "linux-fly-builder",
			App:        app,
			Machine:    env("WEFT_FLY_BUILDER_MACHINE"),
			BaseDir:    envOrDefault(env("WEFT_FLY_BUILDER_BASE"), "/data/weft-builder"),
			GoBin:      envOrDefault(env("WEFT_FLY_BUILDER_GO_BIN"), "/usr/local/go/bin/go"),
			ZigTarget:  env("WEFT_FLY_BUILDER_ZIG_TARGET"),
			ZigVersion: envOrDefault(env("WEFT_FLY_BUILDER_ZIG_VERSION"), "0.13.0"),
		})
	}
	return normalizeBuilders(out), nil
}

func normalizeBuilders(in []config.AgentBuilder) []config.AgentBuilder {
	out := make([]config.AgentBuilder, 0, len(in))
	for _, b := range in {
		nb := b
		nb.Type = strings.ToLower(strings.TrimSpace(nb.Type))
		nb.Name = strings.TrimSpace(nb.Name)
		nb.Host = strings.TrimSpace(nb.Host)
		nb.RemoteDir = strings.TrimSpace(nb.RemoteDir)
		nb.GoBin = strings.TrimSpace(nb.GoBin)
		nb.App = strings.TrimSpace(nb.App)
		nb.Machine = strings.TrimSpace(nb.Machine)
		nb.BaseDir = strings.TrimSpace(nb.BaseDir)
		nb.ZigTarget = strings.TrimSpace(nb.ZigTarget)
		nb.ZigVersion = strings.TrimSpace(nb.ZigVersion)
		switch nb.Type {
		case "ssh":
			if nb.Host == "" {
				continue
			}
			nb.RemoteDir = envOrDefault(nb.RemoteDir, "~/.cache/weft/agent-build")
			nb.GoBin = envOrDefault(nb.GoBin, "/usr/local/go/bin/go")
		case "fly":
			if nb.App == "" || nb.Machine == "" {
				continue
			}
			nb.BaseDir = envOrDefault(nb.BaseDir, "/data/weft-builder")
			nb.GoBin = envOrDefault(nb.GoBin, "/usr/local/go/bin/go")
			nb.ZigVersion = envOrDefault(nb.ZigVersion, "0.13.0")
		default:
			continue
		}
		out = append(out, nb)
	}
	return out
}

func envOrDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func buildViaSSHBuilder(version, goos, goarch, outputPath string, builder config.AgentBuilder) error {
	root, err := RepoRoot()
	if err != nil {
		return fmt.Errorf("locate repo root: %w", err)
	}
	if err := quickCheckSSHBuilder(builder.Host); err != nil {
		return err
	}
	remoteDir := envOrDefault(builder.RemoteDir, "~/.cache/weft/agent-build")
	remoteGo := envOrDefault(builder.GoBin, "/usr/local/go/bin/go")
	remoteOut := filepath.Join(remoteDir, fmt.Sprintf("weft-agent-%s-%s", goos, goarch))

	rsyncSSH := rsyncSSHCommand()
	args := []string{
		"-az", "--delete",
		"-e", rsyncSSH,
		"--exclude=.git/", "--exclude=.jj/", "--exclude=.claude/",
		"--exclude=.gocache/", "--exclude=.gomodcache/", "--exclude=.cache/",
		"--exclude=.bench-*-gocache/", "--exclude=.bench-*-gomodcache/",
		"--exclude=testdata/", "--exclude=dist/",
		"--exclude=internal/agentdeploy/binaries/weft-agent-*",
		"--exclude=internal/agentdeploy/binaries/VERSION",
		"--exclude=weft", "--exclude=placement.test",
		root + "/",
		builder.Host + ":" + remoteDir + "/",
	}
	if out, err := runCommandCapture("", nil, "rsync", args...); err != nil {
		return fmt.Errorf("rsync source: %s", strings.TrimSpace(out))
	}

	buildCmd := fmt.Sprintf("cd %s && CGO_ENABLED=1 GOOS=%s GOARCH=%s %s build -buildvcs=false -ldflags %s -o %s ./cmd/agent",
		shellQuote(remoteDir), goos, goarch, shellQuote(remoteGo),
		shellQuote("-X main.version="+version), shellQuote(remoteOut))
	sshArgs := append(sshBaseArgs(builder.Host), buildCmd)
	if out, err := runCommandCapture("", nil, "ssh", sshArgs...); err != nil {
		return fmt.Errorf("remote build: %s", strings.TrimSpace(out))
	}

	tmp := outputPath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	if out, err := runCommandCapture("", nil, "rsync", "-az", "-e", rsyncSSH, builder.Host+":"+remoteOut, tmp); err != nil {
		return fmt.Errorf("download build: %s", strings.TrimSpace(out))
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod downloaded binary: %w", err)
	}
	if err := os.Rename(tmp, outputPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install binary: %w", err)
	}
	return nil
}

func quickCheckSSHBuilder(host string) error {
	args := append(sshBaseArgs(host), "true")
	if out, err := runCommandCapture("", nil, "ssh", args...); err != nil {
		return fmt.Errorf("connect ssh builder: %s", strings.TrimSpace(out))
	}
	return nil
}

func sshBaseArgs(host string) []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=3",
		"-o", "ConnectionAttempts=1",
		host,
	}
}

func rsyncSSHCommand() string {
	// Keep this in sync with sshBaseArgs for consistent fast-fail behavior.
	return "ssh -o BatchMode=yes -o ConnectTimeout=3 -o ConnectionAttempts=1"
}

func runCommandCapture(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$`") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
