package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/slack"
	"github.com/spf13/cobra"
)

var slackTestMessage string

var slackCmd = &cobra.Command{
	Use:   "slack",
	Short: "Manage Slack integration",
}

var slackTestCmd = &cobra.Command{
	Use:   "test",
	Short: "Send a test message to the configured Slack webhook",
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runSlackTest(cmd.OutOrStdout(), slackTestMessage, slack.GetWebhook, slack.Post)
	},
}

func init() {
	rootCmd.AddCommand(slackCmd)
	slackCmd.AddCommand(slackTestCmd)
	slackTestCmd.Flags().StringVar(&slackTestMessage, "message", "", "Message to send (default: generated test message)")
}

func runSlackTest(out io.Writer, message string, getWebhook func() string, post func(string) error) error {
	if strings.TrimSpace(getWebhook()) == "" {
		return errors.New("Slack webhook is not configured; set WEFT_SLACK_WEBHOOK or SLACK_WEBHOOK in ~/.config/weft/config")
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = defaultSlackTestMessage(time.Now(), os.Hostname)
	}
	if err := post(message); err != nil {
		return fmt.Errorf("send Slack test message: %w", err)
	}
	fmt.Fprintln(out, "Slack test message sent")
	return nil
}

func defaultSlackTestMessage(now time.Time, hostname func() (string, error)) string {
	host, err := hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("weft Slack webhook test from %s at %s", host, now.Format(time.RFC3339))
}
