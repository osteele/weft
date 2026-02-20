// Package slack provides Slack notification configuration for remote-jobs.
package slack

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/osteele/remote-jobs/internal/scripts"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// NotifyScriptPath is where the notification script is deployed on remote hosts.
const NotifyScriptPath = "/tmp/remote-jobs-notify-slack.sh"

// GetWebhook returns the Slack webhook URL from environment or config file.
func GetWebhook() string {
	// Check environment variable first
	if webhook := os.Getenv("REMOTE_JOBS_SLACK_WEBHOOK"); webhook != "" {
		return webhook
	}

	// Check config file
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	configFile := filepath.Join(home, ".config", "remote-jobs", "config")
	content, err := os.ReadFile(configFile)
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "SLACK_WEBHOOK=") {
			return strings.TrimPrefix(line, "SLACK_WEBHOOK=")
		}
	}

	return ""
}

// BuildRunnerEnvPrefix builds the environment variable prefix for the queue runner.
func BuildRunnerEnvPrefix(slackWebhook string) string {
	envVars := ""
	if slackWebhook != "" {
		envVars = fmt.Sprintf("REMOTE_JOBS_SLACK_WEBHOOK='%s' ", slackWebhook)
		if v := os.Getenv("REMOTE_JOBS_SLACK_VERBOSE"); v == "1" {
			envVars += "REMOTE_JOBS_SLACK_VERBOSE=1 "
		}
		if v := os.Getenv("REMOTE_JOBS_SLACK_NOTIFY"); v != "" {
			envVars += fmt.Sprintf("REMOTE_JOBS_SLACK_NOTIFY='%s' ", v)
		}
		if v := os.Getenv("REMOTE_JOBS_SLACK_MIN_DURATION"); v != "" {
			envVars += fmt.Sprintf("REMOTE_JOBS_SLACK_MIN_DURATION='%s' ", v)
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
		return
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(scripts.NotifySlackScript); err != nil {
		tmpFile.Close()
		return
	}
	tmpFile.Close()

	if err := ssh.CopyTo(tmpFile.Name(), host, NotifyScriptPath); err != nil {
		return
	}
	ssh.Run(host, fmt.Sprintf("chmod +x '%s'", NotifyScriptPath))
}
