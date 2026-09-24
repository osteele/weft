package agentdeploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/ssh"
)

const (
	installedAgentIdentitySchemaVersion = 3
	installedAgentIdentitySuffix        = ".agent-version.json"
)

// revisionFallbackPrefix marks an identity that names a revision instead of
// hashing the agent's source bytes.
const revisionFallbackPrefix = "vcs-"

// AgentVersionKind separates the two things an agent identity can be. A source
// hash describes the bytes, so every process that can compute it derives the
// same value from the same tree. A revision fallback only names a commit, so
// it disagrees with the source hash of the very tree it was computed from.
//
// The kinds must stay distinguishable because they are compared across
// processes with different environments: one that can run `go list` produces a
// hash, one that cannot produces a fallback. Left unmarked, that disagreement
// is indistinguishable from a genuinely older build, so the weaker process
// overwrites the stronger one's deployment and re-execs the runner on every
// pass.
type AgentVersionKind int

const (
	// AgentVersionAbsent is an empty identity: nothing was observed.
	AgentVersionAbsent AgentVersionKind = iota
	// AgentVersionSourceHash identifies the agent's source bytes.
	AgentVersionSourceHash
	// AgentVersionRevisionFallback names a revision only.
	AgentVersionRevisionFallback
)

// AgentVersionKindOf classifies an agent identity string.
func AgentVersionKindOf(version string) AgentVersionKind {
	switch {
	case strings.TrimSpace(version) == "":
		return AgentVersionAbsent
	case strings.HasPrefix(version, revisionFallbackPrefix):
		return AgentVersionRevisionFallback
	default:
		return AgentVersionSourceHash
	}
}

type installedAgentIdentity struct {
	SchemaVersion             int      `json:"schema_version"`
	AgentVersion              string   `json:"agent_version"`
	PreparedTargets           []string `json:"prepared_targets"`
	RecordedAt                int64    `json:"recorded_at"`
	ExecutableSize            int64    `json:"executable_size"`
	ExecutableModTimeUnixNano int64    `json:"executable_mod_time_unix_nano"`
}

// RepoRoot returns the root directory of the weft source tree.
// First tries the CWD-based VCS root, validating it contains the weft go.mod.
// Falls back to the compile-time source directory if CWD is in a different repo.
func RepoRoot() (string, error) {
	// Try CWD-based VCS root first
	if root, err := jjWorkspaceRoot(); err == nil {
		if isWeftRoot(root) {
			return root, nil
		}
	}
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		root := strings.TrimSpace(string(out))
		if isWeftRoot(root) {
			return root, nil
		}
	}

	// Fall back to compile-time source directory
	if root := compileTimeRoot(); root != "" && isWeftRoot(root) {
		return root, nil
	}

	return "", fmt.Errorf("weft source tree not found (CWD is in a different repo)")
}

// isWeftRoot checks if a directory is the weft source tree root
// by looking for a go.mod with the weft module path.
func isWeftRoot(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "module github.com/osteele/weft")
}

// compileTimeRoot returns the source tree root based on the file path
// of this source file at compile time (via runtime.Caller).
func compileTimeRoot() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	// thisFile is .../internal/agentdeploy/version.go — go up 3 levels
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	return root
}

func installedAgentIdentityPathForExecutable(executable string) string {
	return executable + installedAgentIdentitySuffix
}

// RecordInstalledAgentIdentity writes the agent fingerprint and prepared
// targets associated with the installed CLI. The sidecar lets that CLI stage a
// matching cached agent binary outside the Weft source checkout.
func RecordInstalledAgentIdentity(version string, preparedTargets []string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate weft executable: %w", err)
	}
	return recordInstalledAgentIdentityAt(
		installedAgentIdentityPathForExecutable(executable),
		executable,
		version,
		preparedTargets,
		time.Now(),
	)
}

func recordInstalledAgentIdentityAt(path, executable, version string, preparedTargets []string, recordedAt time.Time) error {
	if !validAgentVersion(version) {
		return fmt.Errorf("invalid agent version %q", version)
	}
	preparedTargets, err := normalizePreparedTargets(preparedTargets)
	if err != nil {
		return err
	}
	executableInfo, err := os.Stat(executable)
	if err != nil {
		return fmt.Errorf("inspect weft executable: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create installed agent identity directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agent-version-*")
	if err != nil {
		return fmt.Errorf("create installed agent identity: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("set installed agent identity permissions: %w", err)
	}
	if err := json.NewEncoder(tmp).Encode(installedAgentIdentity{
		SchemaVersion:             installedAgentIdentitySchemaVersion,
		AgentVersion:              version,
		PreparedTargets:           preparedTargets,
		RecordedAt:                recordedAt.Unix(),
		ExecutableSize:            executableInfo.Size(),
		ExecutableModTimeUnixNano: executableInfo.ModTime().UnixNano(),
	}); err != nil {
		return fmt.Errorf("encode installed agent identity: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync installed agent identity: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close installed agent identity: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install agent identity: %w", err)
	}
	keep = true
	return nil
}

func readInstalledAgentIdentity(path, executable string) (string, error) {
	identity, err := readInstalledAgentIdentityRecord(path, executable)
	if err != nil {
		return "", err
	}
	return identity.AgentVersion, nil
}

func readInstalledAgentIdentityRecord(path, executable string) (installedAgentIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return installedAgentIdentity{}, err
	}
	var identity installedAgentIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		return installedAgentIdentity{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if identity.SchemaVersion != installedAgentIdentitySchemaVersion {
		return installedAgentIdentity{}, fmt.Errorf(
			"unsupported installed agent identity schema %d in %s",
			identity.SchemaVersion, path)
	}
	if identity.RecordedAt <= 0 {
		return installedAgentIdentity{}, fmt.Errorf("installed agent identity in %s has no recording time", path)
	}
	if !validAgentVersion(identity.AgentVersion) {
		return installedAgentIdentity{}, fmt.Errorf("installed agent identity in %s has invalid version %q", path, identity.AgentVersion)
	}
	if _, err := normalizePreparedTargets(identity.PreparedTargets); err != nil {
		return installedAgentIdentity{}, fmt.Errorf("installed agent identity in %s: %w", path, err)
	}
	executableInfo, err := os.Stat(executable)
	if err != nil {
		return installedAgentIdentity{}, fmt.Errorf("inspect weft executable: %w", err)
	}
	if identity.ExecutableSize != executableInfo.Size() ||
		identity.ExecutableModTimeUnixNano != executableInfo.ModTime().UnixNano() {
		return installedAgentIdentity{}, fmt.Errorf("installed agent identity in %s belongs to a different executable", path)
	}
	return identity, nil
}

func normalizePreparedTargets(targets []string) ([]string, error) {
	normalized := make([]string, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if !validAgentVersion(target) || !strings.Contains(target, "-") {
			return nil, fmt.Errorf("invalid prepared agent target %q", target)
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		normalized = append(normalized, target)
	}
	if len(normalized) == 0 {
		return nil, fmt.Errorf("installed agent identity has no prepared targets")
	}
	sort.Strings(normalized)
	return normalized, nil
}

func validAgentVersion(version string) bool {
	if version == "" || len(version) > 128 {
		return false
	}
	for _, r := range version {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || r == '.' || r == '_' || r == '+' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// LocalAgentVersionForTarget resolves the identity of the agent binary for a
// specific GOOS/GOARCH target. An installed identity applies only to targets
// that were prepared before the identity was recorded.
func LocalAgentVersionForTarget(goos, goarch string) (string, error) {
	return localAgentVersionForTarget(goos + "-" + goarch)
}

func localAgentVersionForTarget(target string) (string, error) {
	repoRoot, rootErr := RepoRoot()
	executable, executableErr := os.Executable()
	return resolveLocalAgentVersionForTarget(repoRoot, rootErr, executable, executableErr, target)
}

// LocalAgentSourceVersion resolves the agent version from the current Weft
// source checkout, ignoring any installed executable identity.
func LocalAgentSourceVersion() (string, error) {
	repoRoot, err := RepoRoot()
	if err != nil {
		return "", err
	}
	return agentVersionFromRepoRoot(repoRoot)
}

func resolveLocalAgentVersionForTarget(repoRoot string, rootErr error, executable string, executableErr error, target string) (string, error) {
	if rootErr == nil {
		if executableErr == nil && !pathWithinRoot(executable, repoRoot) {
			identityPath := installedAgentIdentityPathForExecutable(executable)
			version, identityErr := readInstalledAgentVersionForTarget(identityPath, executable, target)
			if identityErr == nil {
				return version, nil
			}
			slog.Debug(
				"installed agent identity unavailable; using source checkout",
				"component", "agentdeploy",
				"path", identityPath,
				"target", target,
				"error", identityErr,
			)
		}
		return agentVersionFromRepoRoot(repoRoot)
	}

	return installedAgentVersionFallbackForTarget(rootErr, executable, executableErr, target)
}

func agentVersionFromRepoRoot(repoRoot string) (string, error) {
	if version, err := localAgentSourceVersion(repoRoot); err == nil {
		return version, nil
	}

	// A revision identity is marked so a later comparison can tell it apart
	// from a source hash of the same tree rather than reading it as a
	// different build.
	if version, err := jjVersion(repoRoot); err == nil {
		return revisionFallbackPrefix + version, nil
	}

	if version, err := gitVersion(repoRoot); err == nil {
		return revisionFallbackPrefix + version, nil
	}

	return "", fmt.Errorf("cannot compute local agent version for %s", repoRoot)
}

func installedAgentVersionFallbackForTarget(rootErr error, executable string, executableErr error, target string) (string, error) {
	if executableErr != nil {
		return "", fmt.Errorf("%w; installed agent identity unavailable: %v", rootErr, executableErr)
	}
	identityPath := installedAgentIdentityPathForExecutable(executable)
	version, identityErr := readInstalledAgentVersionForTarget(identityPath, executable, target)
	if identityErr != nil {
		return "", fmt.Errorf("%w; installed agent identity unavailable: %v", rootErr, identityErr)
	}
	return version, nil
}

func readInstalledAgentVersionForTarget(path, executable, target string) (string, error) {
	identity, err := readInstalledAgentIdentityRecord(path, executable)
	if err != nil {
		return "", err
	}
	if target != "" && !identityIncludesTarget(identity, target) {
		return "", fmt.Errorf("installed agent identity in %s has no prepared target %s", path, target)
	}
	return identity.AgentVersion, nil
}

func identityIncludesTarget(identity installedAgentIdentity, target string) bool {
	for _, prepared := range identity.PreparedTargets {
		if prepared == target {
			return true
		}
	}
	return false
}

func installedAgentIdentityForCurrentExecutableTarget(goos, goarch string) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate weft executable: %w", err)
	}
	return readInstalledAgentVersionForTarget(
		installedAgentIdentityPathForExecutable(executable),
		executable,
		goos+"-"+goarch,
	)
}

func pathWithinRoot(path, root string) bool {
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(resolvedRoot, resolvedPath)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

type goListPackage struct {
	Dir        string
	GoFiles    []string
	CgoFiles   []string
	SFiles     []string
	EmbedFiles []string
}

func localAgentSourceVersion(repoRoot string) (string, error) {
	files, err := sourceFilesForPackage(repoRoot, "./cmd/agent", "agent", "", "")
	if err != nil {
		return "", err
	}
	return hashVersionFromFiles(repoRoot, files)
}

func localCLISourceVersion(repoRoot, goos, goarch string) (string, error) {
	files, err := sourceFilesForPackage(repoRoot, ".", "CLI", goos, goarch)
	if err != nil {
		return "", err
	}
	return hashVersionFromFiles(repoRoot, files)
}

func agentSourceFiles(repoRoot string) ([]string, error) {
	return sourceFilesForPackage(repoRoot, "./cmd/agent", "agent", "", "")
}

func sourceFilesForPackage(repoRoot, packagePattern, label, goos, goarch string) ([]string, error) {
	// `go list -deps -json` can take 10–20s on a cold build cache. A short
	// budget makes the source hash silently lose to a VCS fallback, which lets
	// uncommitted changes inherit a stale deployed binary's version.
	const goListTimeout = 60 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), goListTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "list", "-deps", "-json", packagePattern)
	cmd.Dir = repoRoot
	if goos != "" || goarch != "" {
		baseEnv := os.Environ()
		targetEnv := make([]string, 0, len(baseEnv)+3)
		for _, entry := range baseEnv {
			if (goos != "" && strings.HasPrefix(entry, "GOOS=")) ||
				(goarch != "" && strings.HasPrefix(entry, "GOARCH=")) ||
				strings.HasPrefix(entry, "CGO_ENABLED=") {
				continue
			}
			targetEnv = append(targetEnv, entry)
		}
		if goos != "" {
			targetEnv = append(targetEnv, "GOOS="+goos)
		}
		if goarch != "" {
			targetEnv = append(targetEnv, "GOARCH="+goarch)
		}
		targetEnv = append(targetEnv, "CGO_ENABLED=1")
		cmd.Env = targetEnv
	}
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("go list %s deps timed out after %s", label, goListTimeout)
		}
		return nil, fmt.Errorf("go list %s deps: %w", label, err)
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	seen := make(map[string]struct{})
	for {
		var pkg goListPackage
		if err := dec.Decode(&pkg); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decode go list %s output: %w", label, err)
		}
		if pkg.Dir == "" {
			continue
		}
		addPackageFiles(repoRoot, pkg.Dir, pkg.GoFiles, seen)
		addPackageFiles(repoRoot, pkg.Dir, pkg.CgoFiles, seen)
		addPackageFiles(repoRoot, pkg.Dir, pkg.SFiles, seen)
		addPackageFiles(repoRoot, pkg.Dir, pkg.EmbedFiles, seen)
	}

	if err := addExistingFile(filepath.Join(repoRoot, "go.mod"), seen); err != nil {
		return nil, err
	}
	if err := addExistingFile(filepath.Join(repoRoot, "go.sum"), seen); err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	files := make([]string, 0, len(seen))
	for file := range seen {
		files = append(files, file)
	}
	sort.Strings(files)
	return files, nil
}

func addPackageFiles(repoRoot, packageDir string, names []string, seen map[string]struct{}) {
	for _, name := range names {
		full := filepath.Clean(filepath.Join(packageDir, name))
		if !isPathWithinRoot(repoRoot, full) {
			continue
		}
		seen[full] = struct{}{}
	}
}

func isPathWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

func addExistingFile(path string, seen map[string]struct{}) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	seen[path] = struct{}{}
	return nil
}

func hashVersionFromFiles(repoRoot string, files []string) (string, error) {
	if len(files) == 0 {
		return "", fmt.Errorf("no source files for version hash")
	}
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	hash := sha256.New()
	for _, file := range sorted {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", file, err)
		}
		rel, err := filepath.Rel(repoRoot, file)
		if err != nil {
			return "", fmt.Errorf("compute relative path for %s: %w", file, err)
		}
		rel = filepath.ToSlash(rel)
		if _, err := hash.Write([]byte(rel)); err != nil {
			return "", err
		}
		if _, err := hash.Write([]byte{0}); err != nil {
			return "", err
		}
		if _, err := hash.Write(data); err != nil {
			return "", err
		}
		if _, err := hash.Write([]byte{0}); err != nil {
			return "", err
		}
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	return sum[:12], nil
}

// agentSourcePaths are the directories whose changes affect the agent binary.
var agentSourcePaths = []string{"cmd/agent/", "internal/"}

func jjVersion(repoRoot string) (string, error) {
	// Find the most recent committed ancestor that touched agent source files.
	// Use @- (parent of working copy) to exclude the working copy itself,
	// because the working copy gets a new commit_id on every jj snapshot,
	// which would cause perpetual version mismatches and agent redeploys.
	args := []string{
		"log", "--no-graph",
		"-r", "ancestors(@-, 200)",
		"-T", `commit_id.short(12)`,
		"--limit", "1",
	}
	args = append(args, agentSourcePaths...)
	if version, err := jjLog(repoRoot, args...); err == nil && version != "" {
		return version, nil
	}

	// Fallback: no ancestor touched agent paths (e.g., brand new repo).
	return jjLog(repoRoot, "log", "--no-graph", "-r", "@-", "-T", `commit_id.short(12)`)
}

func jjWorkspaceRoot() (string, error) {
	out, err := exec.Command("jj", "workspace", "root").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// jjLog runs a jj command in repoRoot and returns the trimmed output.
func jjLog(repoRoot string, args ...string) (string, error) {
	cmd := exec.Command("jj", args...)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		return "", fmt.Errorf("empty jj commit id")
	}
	return version, nil
}

func gitVersion(repoRoot string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--short=12", "HEAD")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		return "", fmt.Errorf("empty git commit id")
	}
	return version, nil
}

const remoteAgentPath = "~/.cache/weft/bin/weft-agent"

// ErrAgentIncompatible is returned by RemoteAgentVersion when the agent binary
// exists on the remote host but fails to run (e.g. glibc version mismatch).
// Callers should treat this the same as "not installed" and redeploy.
var ErrAgentIncompatible = errors.New("agent binary incompatible")

// RemoteAgentVersion runs the agent binary on the remote host and parses
// the version string. Returns empty string if the agent is not installed.
// Returns ErrAgentIncompatible (wrapping the output) if the binary exists but
// fails to run (e.g. glibc mismatch) — callers should redeploy in that case.
func RemoteAgentVersion(host string) (string, error) {
	// Use a two-step check: first test if the binary exists, then run it.
	// This distinguishes "not installed" (return "") from "exists but crashes"
	// (return ErrAgentIncompatible), so callers can rebuild or redeploy it.
	checkCmd := fmt.Sprintf(
		`if [ ! -f %s ]; then echo "not-installed"; exit 0; fi; %s --version 2>&1`,
		remoteAgentPath, remoteAgentPath,
	)
	stdout, _, err := ssh.Run(host, checkCmd)
	if err != nil {
		// If this is an SSH connection error, we can't tell anything about the binary.
		if ssh.IsConnectionError(err.Error()) {
			return "", fmt.Errorf("remote agent version: %w", err)
		}
		// Non-zero exit: the binary exists but crashed (e.g. glibc mismatch).
		// stdout contains the crash output from 2>&1.
		output := strings.TrimSpace(stdout)
		return "", fmt.Errorf("%w on %s: %s", ErrAgentIncompatible, host, output)
	}
	output := strings.TrimSpace(stdout)
	if output == "not-installed" {
		return "", nil
	}
	ver := parseAgentVersionOutput(output)
	if ver == "" && output != "" {
		// Binary exists but didn't print a recognizable version — likely a
		// runtime error such as a glibc version mismatch.
		return "", fmt.Errorf("%w on %s: %s", ErrAgentIncompatible, host, output)
	}
	return ver, nil
}

// parseAgentVersionOutput extracts the version from "weft-agent <version>" output.
// Returns empty string if the output doesn't match the expected format.
func parseAgentVersionOutput(output string) string {
	if output == "" {
		return ""
	}
	parts := strings.Fields(output)
	if len(parts) >= 2 && parts[0] == "weft-agent" {
		return parts[1]
	}
	return ""
}
