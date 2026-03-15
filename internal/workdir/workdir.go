package workdir

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/config"
)

var repoRootResolver = detectRepoRoot

// ProjectDir resolves the local project directory for a submission.
// It prefers the enclosing repo root (jj, repo, git) and falls back to the
// provided directory, or the current working directory when dir is empty.
func ProjectDir(dir string) (string, error) {
	localDir, err := localProjectDir(dir)
	if err != nil {
		return "", err
	}
	if localDir == "" {
		return "", nil
	}
	if root := repoRootResolver(localDir); root != "" {
		return root, nil
	}
	return localDir, nil
}

// ResolveProjectName returns the explicit project name when provided, otherwise
// derives it from the repo root (or working directory fallback).
func ResolveProjectName(project, dir string) (string, error) {
	project = strings.TrimSpace(project)
	if project != "" && project != "." {
		return project, nil
	}
	projectDir, err := ProjectDir(dir)
	if err != nil {
		return "", err
	}
	if projectDir == "" {
		return "", nil
	}
	return filepath.Base(projectDir), nil
}

// ProjectName derives the project name from the submission directory.
func ProjectName(dir string) string {
	name, err := ResolveProjectName("", dir)
	if err != nil {
		return ""
	}
	return name
}

// ResolveLocal converts a tilde-prefixed working directory back to a local
// absolute path using the automap directory prefixes. Returns "" if the path
// cannot be resolved to a local directory.
func ResolveLocal(workingDir string) string {
	if workingDir == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	for _, prefix := range config.AutomapDirs() {
		expanded := expandTilde(prefix, home)
		if strings.HasPrefix(workingDir, prefix+"/") {
			rel := workingDir[len(prefix)+1:]
			return filepath.Join(expanded, rel)
		}
		if workingDir == prefix {
			return expanded
		}
	}
	if filepath.IsAbs(workingDir) {
		return workingDir
	}
	return ""
}

// expandTilde replaces a leading "~" in prefix with the given home directory.
func expandTilde(prefix, home string) string {
	return strings.Replace(prefix, "~", home, 1)
}

func localProjectDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get current directory: %w", err)
		}
		return filepath.Clean(cwd), nil
	}
	if resolved := ResolveLocal(dir); resolved != "" {
		return filepath.Clean(resolved), nil
	}
	if strings.HasPrefix(dir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("get home directory: %w", err)
		}
		return filepath.Clean(expandTilde(dir, home)), nil
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir), nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path for %q: %w", dir, err)
	}
	return filepath.Clean(abs), nil
}

func detectRepoRoot(dir string) string {
	for _, spec := range []struct {
		name string
		args []string
	}{
		{name: "jj", args: []string{"root"}},
		{name: "repo", args: []string{"root"}},
		{name: "git", args: []string{"rev-parse", "--show-toplevel"}},
	} {
		cmd := exec.Command(spec.name, spec.args...)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		root := strings.TrimSpace(string(out))
		if root != "" {
			return filepath.Clean(root)
		}
	}
	return ""
}
