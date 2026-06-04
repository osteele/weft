package agentdeploy

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/ssh"
)

const rcloneConfigPath = "~/.config/rclone/rclone.conf"

// EnsureRcloneConfig ensures the remote host has an rclone config with the [r2] section.
// If the config already has [r2], this is a no-op. Otherwise it creates/appends the section.
func EnsureRcloneConfig(host string, r2Cfg cloud.R2Config) error {
	// Check if [r2] section already exists
	checkCmd := fmt.Sprintf("grep -q '^\\[r2\\]' %s 2>/dev/null && echo YES || echo NO", rcloneConfigPath)
	stdout, _, err := ssh.Run(host, checkCmd)
	if err != nil {
		return fmt.Errorf("check rclone config on %s: %w", host, err)
	}

	if strings.TrimSpace(stdout) == "YES" {
		return nil
	}

	// Generate and write config. Use stdin instead of a heredoc so this works
	// through the SSH session pool and for secrets with shell-sensitive bytes.
	configContent := cloud.GenerateRcloneConfig(r2Cfg)
	writeCmd := fmt.Sprintf("mkdir -p ~/.config/rclone && cat >> %s", rcloneConfigPath)
	if _, stderr, err := ssh.RunWithStdin(host, writeCmd, configContent); err != nil {
		if s := strings.TrimSpace(stderr); s != "" {
			return fmt.Errorf("write rclone config on %s: %s", host, s)
		}
		return fmt.Errorf("write rclone config on %s: %w", host, err)
	}

	return nil
}
