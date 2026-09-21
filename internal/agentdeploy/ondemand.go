package agentdeploy

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ssh"
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
	version, err := LocalAgentVersionForTarget(goos, goarch)
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

func buildOnDemand(version, goos, goarch, outputPath string, output io.Writer, onProgress BuildProgressFunc) error {
	if onProgress == nil {
		onProgress = func(string) {}
	}
	builders, err := resolveBuilders(goos, goarch)
	if err != nil {
		return err
	}
	if len(builders) == 0 {
		return fmt.Errorf("%w for %s/%s: no builders configured in ~/.config/weft/config.toml [agent_build] and no WEFT_* fallback vars found in the environment, the repo .envrc/.env, or ~/.config/weft/fly-builder.env",
			ErrAgentNotAvailable, goos, goarch)
	}

	var attempts []string
	for _, builder := range builders {
		err := runBuilder(version, goos, goarch, outputPath, builder, output, onProgress)
		if err == nil {
			// Report what was tried and failed even though this one worked.
			// Discarding it is how a builder that has been broken for months
			// stays invisible: every run still produces a binary, so nothing
			// ever says which builder produced it or that another is down.
			for _, attempt := range attempts {
				fmt.Fprintf(output, "warning: agent builder failed but a later builder succeeded: %s\n", attempt)
			}
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

func runBuilder(version, goos, goarch, outputPath string, builder config.AgentBuilder, output io.Writer, onProgress BuildProgressFunc) error {
	switch strings.ToLower(strings.TrimSpace(builder.Type)) {
	case "ssh":
		return buildViaSSHBuilder(version, goos, goarch, outputPath, builder, output, onProgress)
	case "fly":
		var captured bytes.Buffer
		_, err := BuildViaFlyBuilderWithProgress(version, goos, goarch, outputPath, builder, io.MultiWriter(&captured, output), onProgress)
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
	defaultIdentity := ""
	if err == nil && cfg != nil {
		defaultIdentity = cfg.Cloud.SSH.ExpandedIdentityFile()
	}
	if err == nil && cfg != nil && len(cfg.AgentBuild.LinuxAMD64Builders) > 0 {
		return normalizeBuilders(cfg.AgentBuild.LinuxAMD64Builders, defaultIdentity), nil
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
		// A builder receives a full source sync and runs compiles, so the host
		// must have been chosen deliberately. An explicitly configured host is
		// deliberate by definition; anything weft derived on its own must be an
		// inventory host. The guard exists for a future path that defaults or
		// falls back to a reachable host, which is how build load would reach
		// infrastructure nobody chose.
		//
		// A refused builder is skipped rather than fatal. Builders are
		// independent, and letting one bad entry disable the others would turn
		// a degraded configuration into no builds at all.
		if reason := builderHostRefusal(host, true); reason != "" {
			fmt.Fprintf(os.Stderr,
				"warning: skipping ssh agent builder %q: %s\n", host, reason)
		} else {
			out = append(out, config.AgentBuilder{
				Type:         "ssh",
				Name:         "linux-ssh-builder",
				Host:         host,
				RemoteDir:    envOrDefault(env("WEFT_LINUX_BUILDER_DIR"), "~/.cache/weft/agent-build"),
				GoBin:        envOrDefault(env("WEFT_LINUX_BUILDER_GO"), "/usr/local/go/bin/go"),
				IdentityFile: envOrDefault(env("WEFT_LINUX_BUILDER_IDENTITY_FILE"), defaultIdentity),
			})
		}
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
	return normalizeBuilders(out, defaultIdentity), nil
}

func normalizeBuilders(in []config.AgentBuilder, defaultIdentity string) []config.AgentBuilder {
	out := make([]config.AgentBuilder, 0, len(in))
	for _, b := range in {
		nb := b
		nb.Type = strings.ToLower(strings.TrimSpace(nb.Type))
		nb.Name = strings.TrimSpace(nb.Name)
		nb.Host = strings.TrimSpace(nb.Host)
		nb.RemoteDir = strings.TrimSpace(nb.RemoteDir)
		nb.GoBin = strings.TrimSpace(nb.GoBin)
		nb.IdentityFile = strings.TrimSpace(nb.IdentityFile)
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
			nb.IdentityFile = config.ExpandUserPath(envOrDefault(nb.IdentityFile, defaultIdentity))
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

func buildViaSSHBuilder(version, goos, goarch, outputPath string, builder config.AgentBuilder, output io.Writer, onProgress BuildProgressFunc) error {
	if onProgress == nil {
		onProgress = func(string) {}
	}
	if output == nil {
		output = io.Discard
	}
	fmt.Fprintf(output, "Building via ssh builder %s...\n", builder.Host)
	root, err := RepoRoot()
	if err != nil {
		return fmt.Errorf("locate repo root: %w", err)
	}
	onProgress("connecting to ssh builder")
	if err := quickCheckSSHBuilder(builder, moduleGoVersion()); err != nil {
		return err
	}
	remoteDir := envOrDefault(builder.RemoteDir, "~/.cache/weft/agent-build")
	remoteGo := envOrDefault(builder.GoBin, "/usr/local/go/bin/go")
	remoteOut := filepath.Join(remoteDir, fmt.Sprintf("weft-agent-%s-%s", goos, goarch))

	rsyncSSH := rsyncSSHCommand(builder)
	// rsync will not create a destination's parent chain, so a host that has
	// never built — or whose cache was cleaned — fails here rather than
	// bootstrapping. Create it first.
	mkdirArgs := append(sshBaseArgs(builder), "mkdir -p "+shellQuote(remoteDir))
	if out, err := runCommandCapture("", nil, "ssh", mkdirArgs...); err != nil {
		return fmt.Errorf("create build directory on %s: %s", builder.Host, strings.TrimSpace(out))
	}

	onProgress("syncing source")
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

	onProgress("building")
	buildCmd := fmt.Sprintf("cd %s && CGO_ENABLED=1 GOOS=%s GOARCH=%s %s build -buildvcs=false -ldflags %s -o %s ./cmd/agent",
		shellQuote(remoteDir), goos, goarch, shellQuote(remoteGo),
		shellQuote("-X main.version="+version), shellQuote(remoteOut))
	sshArgs := append(sshBaseArgs(builder), buildCmd)
	if out, err := runCommandCapture("", nil, "ssh", sshArgs...); err != nil {
		return fmt.Errorf("remote build: %s", strings.TrimSpace(out))
	}

	tmp := outputPath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	onProgress("downloading")
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

// quickCheckSSHBuilder verifies the builder can actually build before anything
// is sent to it.
//
// Reachability was never the failing condition. A builder can answer `ssh host
// true` while having no room for the sources, no toolchain able to compile this
// module, or a toolchain that must download hundreds of megabytes first. Each
// of those is one round trip to detect and, undetected, is paid as a full
// source sync followed by a failure.
func quickCheckSSHBuilder(builder config.AgentBuilder, goVersion string) error {
	goBin := envOrDefault(builder.GoBin, "/usr/local/go/bin/go")
	// Probe $HOME rather than the build directory: the build directory may not
	// exist yet on a host that has never built, and df on a missing path prints
	// nothing, which would fail the probe for a host that is perfectly able to
	// build.
	probe := fmt.Sprintf(
		"df -Pk \"$HOME\" 2>/dev/null | awk 'NR==2{print $4}'; %s version 2>&1 || echo NO_TOOLCHAIN",
		shellQuote(goBin))
	args := append(sshBaseArgs(builder), probe)
	out, err := runCommandCapture("", nil, "ssh", args...)
	if err != nil {
		return fmt.Errorf("connect ssh builder: %s", strings.TrimSpace(out))
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return fmt.Errorf("ssh builder %s did not answer the capability probe: %q",
			builder.Host, strings.TrimSpace(out))
	}
	freeKB, convErr := strconv.ParseInt(strings.TrimSpace(lines[0]), 10, 64)
	if convErr == nil && freeKB < minBuilderFreeKB {
		return fmt.Errorf(
			"ssh builder %s has %.1f GB free, below the %.1f GB a build of this module needs; "+
				"refusing to sync sources it cannot compile",
			builder.Host, float64(freeKB)/1048576, float64(minBuilderFreeKB)/1048576)
	}
	toolchain := strings.TrimSpace(lines[len(lines)-1])
	if strings.Contains(toolchain, "NO_TOOLCHAIN") {
		return fmt.Errorf("ssh builder %s has no Go toolchain at %s", builder.Host, goBin)
	}
	if goVersion != "" && !toolchainSatisfies(toolchain, goVersion) {
		return fmt.Errorf(
			"ssh builder %s reports %q but this module needs go %s; it would download a full "+
				"toolchain on every cold build",
			builder.Host, toolchain, goVersion)
	}
	return nil
}

// minBuilderFreeKB is the headroom a build of this module needs beyond whatever
// caches already exist.
//
// Calibrated to catch the condition that actually caused harm — a builder run
// against a nearly full disk, which fails after the sources have been sent —
// without refusing a host that can build. A host with warm module and compile
// caches and a matching toolchain needs far less than a cold one; the toolchain
// probe covers the cold case separately, since a mismatched toolchain is what
// turns a build into a several-hundred-megabyte download.
const minBuilderFreeKB = 1024 * 1024

// toolchainSatisfies reports whether a remote `go version` line is at least the
// version this module requires. A lower toolchain still builds, by downloading
// the required one first, which is a large per-cold-build cost rather than a
// working configuration.
func toolchainSatisfies(versionLine, required string) bool {
	fields := strings.Fields(versionLine)
	if len(fields) < 3 || !strings.HasPrefix(fields[2], "go") {
		return true
	}
	return !semverLess(strings.TrimPrefix(fields[2], "go"), required)
}

func semverLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])
		if aerr != nil || berr != nil {
			return false
		}
		if an != bn {
			return an < bn
		}
	}
	return false
}

const builderSSHTimeout = 3 * time.Second

func builderSSHOptionArgs(builder config.AgentBuilder) []string {
	args := ssh.BatchModeArgs(builderSSHTimeout, "ConnectionAttempts=1")
	args = append(args, "-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes")
	if identity := config.ExpandUserPath(strings.TrimSpace(builder.IdentityFile)); identity != "" {
		args = append(args, "-i", identity)
	}
	return args
}

func sshBaseArgs(builder config.AgentBuilder) []string {
	return append(builderSSHOptionArgs(builder), builder.Host)
}

func rsyncSSHCommand(builder config.AgentBuilder) string {
	args := builderSSHOptionArgs(builder)
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return "ssh " + strings.Join(quoted, " ")
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

// moduleGoVersion reads the go directive from this module's go.mod, which is
// the toolchain a builder must be able to satisfy without downloading one.
func moduleGoVersion() string {
	root, err := RepoRoot()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "go" {
			return fields[1]
		}
	}
	return ""
}

// builderHostRefusal reports why a host must not be used as a build host, or
// "" when it is acceptable.
//
// explicit means an operator named this host in configuration. That is a
// deliberate choice and is honoured even for a host weft does not otherwise
// manage — the operator accepts responsibility for its disk and toolchain, and
// the disk and toolchain probes still run before anything is sent to it.
//
// A host weft selected for itself must be in the inventory, so that a default,
// a fallback, or an expansion to "the first reachable host" cannot quietly turn
// unrelated infrastructure into a build host.
func builderHostRefusal(host string, explicit bool) string {
	if explicit {
		return ""
	}
	resolved := resolveSSHHostName(host)
	for _, candidate := range []string{host, resolved} {
		if candidate != "" && inventory.FindHost(candidate) != nil {
			return ""
		}
	}
	if resolved != "" && resolved != host {
		return fmt.Sprintf(
			"it was not explicitly configured and resolves to %q, which is not in the host "+
				"inventory (~/.config/weft/hosts/)", resolved)
	}
	return "it was not explicitly configured and is not in the host inventory " +
		"(~/.config/weft/hosts/)"
}

// resolveSSHHostName returns the HostName an ssh alias expands to.
func resolveSSHHostName(host string) string {
	out, err := runCommandCapture("", nil, "ssh", "-G", host)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "hostname" {
			return fields[1]
		}
	}
	return ""
}
