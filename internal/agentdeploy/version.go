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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/ssh"
)

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

// LocalAgentVersion returns a stable version string for the agent binary.
// It prefers a deterministic source hash computed from the cmd/agent build
// inputs, plus go.mod/go.sum. If hashing fails (for example, go is unavailable),
// it falls back to VCS-derived versions.
func LocalAgentVersion() (string, error) {
	repoRoot, err := RepoRoot()
	if err != nil {
		return "", err
	}

	if version, err := localAgentSourceVersion(repoRoot); err == nil {
		return version, nil
	}

	if version, err := jjVersion(repoRoot); err == nil {
		return version, nil
	}

	if version, err := gitVersion(repoRoot); err == nil {
		return version, nil
	}

	return "", fmt.Errorf("cannot compute local agent version for %s", repoRoot)
}

type goListPackage struct {
	Dir        string
	GoFiles    []string
	CgoFiles   []string
	SFiles     []string
	EmbedFiles []string
}

func localAgentSourceVersion(repoRoot string) (string, error) {
	files, err := agentSourceFiles(repoRoot)
	if err != nil {
		return "", err
	}
	return hashVersionFromFiles(repoRoot, files)
}

func agentSourceFiles(repoRoot string) ([]string, error) {
	// `go list -deps -json ./cmd/agent` walks every transitive dependency
	// and is regularly 10–20s on a cold build cache (Apple Silicon, no GOCACHE
	// hits). With a 3-second budget the source-hash version path always lost
	// to jjVersion in normal development, which meant uncommitted agent
	// changes inherited the parent commit's version and `agentdeploy.EnsureBuilt`
	// happily served a cached stale binary instead of rebuilding. 60s is
	// generous but still bounded, and the timeout only matters on the rare
	// first invocation per cache state.
	const goListTimeout = 60 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), goListTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "list", "-deps", "-json", "./cmd/agent")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("go list agent deps timed out after %s", goListTimeout)
		}
		return nil, fmt.Errorf("go list agent deps: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	seen := make(map[string]struct{})
	for {
		var pkg goListPackage
		if err := dec.Decode(&pkg); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decode go list output: %w", err)
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
		return "", fmt.Errorf("no source files for agent version hash")
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
