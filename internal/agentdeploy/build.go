package agentdeploy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CachePath returns the local cache path for a built agent binary.
// Layout: ~/.cache/weft/builds/<version>/<goos>-<goarch>/weft-agent
func CachePath(version, goos, goarch string) string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = filepath.Join(os.Getenv("HOME"), ".cache")
	}
	return filepath.Join(cacheDir, "weft", "builds", version, goos+"-"+goarch, "weft-agent")
}

// moduleRoot returns the root directory of the Go module by locating go.mod.
func moduleRoot() (string, error) {
	cmd := exec.Command("go", "env", "GOMOD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("not inside a Go module")
	}
	return filepath.Dir(gomod), nil
}

// EnsureBuilt checks the local build cache and cross-compiles the agent
// if the cached binary is missing. Returns the path to the built binary.
func EnsureBuilt(version, goos, goarch string) (string, error) {
	path := CachePath(version, goos, goarch)

	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}

	root, err := moduleRoot()
	if err != nil {
		return "", fmt.Errorf("find module root: %w", err)
	}

	ldflags := fmt.Sprintf("-X main.version=%s", version)
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", path, "./cmd/agent")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		os.Remove(path) // clean up partial build
		return "", fmt.Errorf("cross-compile agent for %s/%s: %w", goos, goarch, err)
	}

	return path, nil
}
