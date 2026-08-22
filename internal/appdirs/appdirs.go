// Package appdirs resolves Weft's XDG base directories.
package appdirs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const appName = "weft"

// StateDir returns the directory for persistent operational state.
func StateDir() (string, error) {
	return resolveDir(os.Getenv("XDG_STATE_HOME"), ".local/state")
}

// DataDir returns the directory for durable user data.
func DataDir() (string, error) {
	return resolveDir(os.Getenv("XDG_DATA_HOME"), ".local/share")
}

func resolveDir(override, fallback string) (string, error) {
	override = strings.TrimSpace(override)
	if override != "" && filepath.IsAbs(override) {
		return filepath.Join(override, appName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find user home directory: %w", err)
	}
	return filepath.Join(home, filepath.FromSlash(fallback), appName), nil
}
