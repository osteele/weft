// Package slack provides Slack notification configuration for weft.
package slack

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/scripts"
	"github.com/osteele/weft/internal/ssh"
)

// NotifyScriptPath is where the notification script is deployed on remote hosts.
const NotifyScriptPath = "~/.cache/weft/bin/notify-slack.sh"

// GetWebhook returns the Slack webhook URL from environment or config file.
func GetWebhook() string {
	return getConfigValue("WEFT_SLACK_WEBHOOK", "SLACK_WEBHOOK")
}

// getConfigValue reads a value from an environment variable first, then from
// the weft config file (~/.config/weft/config) using the fileKey.
func getConfigValue(envVar, fileKey string) string {
	if v := os.Getenv(envVar); v != "" {
		return v
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	content, err := os.ReadFile(filepath.Join(home, ".config", "weft", "config"))
	if err != nil {
		return ""
	}

	prefix := fileKey + "="
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}

	return ""
}

// BuildRunnerEnvPrefix builds the environment variable prefix for the queue runner.
func BuildRunnerEnvPrefix(slackWebhook string) string {
	envVars := ""
	if slackWebhook != "" {
		envVars = fmt.Sprintf("WEFT_SLACK_WEBHOOK='%s' ", slackWebhook)
		if v := os.Getenv("WEFT_SLACK_VERBOSE"); v == "1" {
			envVars += "WEFT_SLACK_VERBOSE=1 "
		}
		if v := os.Getenv("WEFT_SLACK_NOTIFY"); v != "" {
			envVars += fmt.Sprintf("WEFT_SLACK_NOTIFY='%s' ", v)
		}
		if v := os.Getenv("WEFT_SLACK_MIN_DURATION"); v != "" {
			envVars += fmt.Sprintf("WEFT_SLACK_MIN_DURATION='%s' ", v)
		}
	}
	return envVars
}

// DeployNotifyScript deploys the Slack notification script to the remote host.
// Uses SCP instead of a heredoc because the SSH session pool wraps commands
// in a subshell, which breaks heredoc delimiter matching.
func DeployNotifyScript(host, slackWebhook string) {
	if slackWebhook == "" {
		return
	}

	tmpFile, err := os.CreateTemp("", "notify-slack-*.sh")
	if err != nil {
		slog.Warn("failed to create temp file for notify script", "component", "slack", "error", err)
		return
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(scripts.NotifySlackScript); err != nil {
		slog.Warn("failed to write notify script", "component", "slack", "error", err)
		tmpFile.Close()
		return
	}
	tmpFile.Close()

	if _, stderr, err := ssh.Run(host, "mkdir -p ~/.cache/weft/bin"); err != nil {
		slog.Warn("failed to create remote notify script directory", "component", "slack", "host", host, "error", err, "stderr", strings.TrimSpace(stderr))
		return
	}

	if err := ssh.CopyTo(tmpFile.Name(), host, NotifyScriptPath); err != nil {
		slog.Warn("failed to deploy notify script", "component", "slack", "host", host, "error", err)
		return
	}
	ssh.Run(host, fmt.Sprintf("chmod +x '%s'", NotifyScriptPath))
}
