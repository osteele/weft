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

// NotifyConfigPath is the remote, agent-local notification credential file.
// The queue runner does not inherit its contents.
const NotifyConfigPath = "~/.config/weft/notify-slack.env"

const notifyConfigTempPath = NotifyConfigPath + ".tmp"

var (
	runSSHFunc = ssh.Run
	copyToFunc = ssh.CopyTo
)

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

// BuildRunnerEnvPrefix builds the non-secret notification environment for the
// queue runner. The webhook is read by the notification helper from its
// restricted configuration file instead of being inherited by the runner.
func BuildRunnerEnvPrefix() string {
	envVars := ""
	if v := os.Getenv("WEFT_SLACK_VERBOSE"); v == "1" {
		envVars += "WEFT_SLACK_VERBOSE=1 "
	}
	if v := os.Getenv("WEFT_SLACK_NOTIFY"); v != "" {
		envVars += fmt.Sprintf("WEFT_SLACK_NOTIFY='%s' ", v)
	}
	if v := os.Getenv("WEFT_SLACK_MIN_DURATION"); v != "" {
		envVars += fmt.Sprintf("WEFT_SLACK_MIN_DURATION='%s' ", v)
	}
	return envVars
}

// DeployNotifyScript deploys the Slack notification script and its restricted
// credential file to the remote host. An empty webhook removes the managed
// credential file.
// Uses SCP instead of a heredoc because the SSH session pool wraps commands
// in a subshell, which breaks heredoc delimiter matching.
func DeployNotifyScript(host, slackWebhook string) {
	if slackWebhook == "" {
		if _, stderr, err := runSSHFunc(host, "rm -f "+NotifyConfigPath+" "+notifyConfigTempPath); err != nil {
			slog.Warn("failed to remove remote notification config", "component", "slack", "host", host, "error", err, "stderr", strings.TrimSpace(stderr))
		}
		return
	}
	if strings.ContainsAny(slackWebhook, "\r\n") {
		slog.Warn("refusing to deploy malformed Slack webhook", "component", "slack", "host", host)
		return
	}

	if _, stderr, err := runSSHFunc(host, "mkdir -p ~/.cache/weft/bin ~/.config/weft && chmod 700 ~/.config/weft"); err != nil {
		slog.Warn("failed to create remote notification directories", "component", "slack", "host", host, "error", err, "stderr", strings.TrimSpace(stderr))
		return
	}

	scriptFile, err := os.CreateTemp("", "notify-slack-*.sh")
	if err != nil {
		slog.Warn("failed to create temp file for notify script", "component", "slack", "error", err)
		return
	}
	defer os.Remove(scriptFile.Name())

	if _, err := scriptFile.Write(scripts.NotifySlackScript); err != nil {
		slog.Warn("failed to write notify script", "component", "slack", "error", err)
		scriptFile.Close()
		return
	}
	scriptFile.Close()

	if err := copyToFunc(scriptFile.Name(), host, NotifyScriptPath); err != nil {
		slog.Warn("failed to deploy notify script", "component", "slack", "host", host, "error", err)
		return
	}
	if _, stderr, err := runSSHFunc(host, "chmod 755 "+NotifyScriptPath); err != nil {
		slog.Warn("failed to set notify script permissions", "component", "slack", "host", host, "error", err, "stderr", strings.TrimSpace(stderr))
		return
	}

	configFile, err := os.CreateTemp("", "weft-notify-slack-*.env")
	if err != nil {
		slog.Warn("failed to create temp notification config", "component", "slack", "error", err)
		return
	}
	defer os.Remove(configFile.Name())
	if _, err := fmt.Fprintf(configFile, "WEFT_SLACK_WEBHOOK=%s\n", slackWebhook); err != nil {
		slog.Warn("failed to write notification config", "component", "slack", "error", err)
		configFile.Close()
		return
	}
	if err := configFile.Close(); err != nil {
		slog.Warn("failed to close notification config", "component", "slack", "error", err)
		return
	}

	if _, stderr, err := runSSHFunc(host, "umask 077; : > "+notifyConfigTempPath); err != nil {
		slog.Warn("failed to prepare remote notification config", "component", "slack", "host", host, "error", err, "stderr", strings.TrimSpace(stderr))
		return
	}
	if err := copyToFunc(configFile.Name(), host, notifyConfigTempPath); err != nil {
		slog.Warn("failed to deploy notification config", "component", "slack", "host", host, "error", err)
		return
	}
	if _, stderr, err := runSSHFunc(host, "chmod 600 "+notifyConfigTempPath+" && mv -f "+notifyConfigTempPath+" "+NotifyConfigPath); err != nil {
		slog.Warn("failed to activate remote notification config", "component", "slack", "host", host, "error", err, "stderr", strings.TrimSpace(stderr))
	}
}
