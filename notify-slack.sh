#!/bin/bash
#
# Send Slack notification for job completion
# This script is designed to run ON THE REMOTE HOST after a job completes.
#
# Usage:
#   notify-slack.sh <session-name> <exit-code> <host>
#
# Configuration:
#   Set REMOTE_JOBS_SLACK_WEBHOOK environment variable, or
#   Create ~/.config/remote-jobs/config with: SLACK_WEBHOOK=https://hooks.slack.com/...
#

set -euo pipefail

if [ $# -lt 3 ]; then
    echo "Usage: $0 <session-name> <exit-code> <host>"
    exit 1
fi

SESSION_NAME="$1"
EXIT_CODE="$2"
HOST="$3"

# Get webhook URL from environment or config file
WEBHOOK_URL="${REMOTE_JOBS_SLACK_WEBHOOK:-}"

if [ -z "$WEBHOOK_URL" ] && [ -f ~/.config/remote-jobs/config ]; then
    WEBHOOK_URL=$(grep '^SLACK_WEBHOOK=' ~/.config/remote-jobs/config 2>/dev/null | cut -d= -f2- || true)
fi

if [ -z "$WEBHOOK_URL" ]; then
    # No webhook configured, silently exit
    exit 0
fi

# Calculate duration if metadata exists
METADATA_FILE="/tmp/tmux-${SESSION_NAME}.meta"
duration_text=""
if [ -f "$METADATA_FILE" ]; then
    start_time=$(grep '^start_time=' "$METADATA_FILE" | cut -d= -f2- || true)
    if [ -n "$start_time" ]; then
        end_time=$(date +%s)
        duration_secs=$((end_time - start_time))
        hours=$((duration_secs / 3600))
        minutes=$(((duration_secs % 3600) / 60))
        seconds=$((duration_secs % 60))
        if [ $hours -gt 0 ]; then
            duration_text=" (${hours}h ${minutes}m ${seconds}s)"
        elif [ $minutes -gt 0 ]; then
            duration_text=" (${minutes}m ${seconds}s)"
        else
            duration_text=" (${seconds}s)"
        fi
    fi
fi

# Set status emoji and text
if [ "$EXIT_CODE" -eq 0 ]; then
    status_emoji=":white_check_mark:"
    status_text="completed successfully"
else
    status_emoji=":x:"
    status_text="failed with exit code $EXIT_CODE"
fi

# Send Slack notification
curl -s -X POST -H 'Content-type: application/json' \
    --data "{\"text\":\"$status_emoji Job *$SESSION_NAME* on \`$HOST\` $status_text$duration_text\"}" \
    "$WEBHOOK_URL" > /dev/null 2>&1 || true
