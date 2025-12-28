#!/bin/bash
#
# Send Slack notification for job completion
# This script is designed to run ON THE REMOTE HOST after a job completes.
#
# Usage:
#   notify-slack.sh <session-name> <exit-code> <host> [metadata-file]
#
# Configuration:
#   Set REMOTE_JOBS_SLACK_WEBHOOK environment variable, or
#   Create ~/.config/remote-jobs/config with: SLACK_WEBHOOK=https://hooks.slack.com/...
#
# Environment Variables:
#   REMOTE_JOBS_SLACK_WEBHOOK     Slack webhook URL (required)
#   REMOTE_JOBS_SLACK_NOTIFY      When to notify: "all" (default), "failures", "none"
#   REMOTE_JOBS_SLACK_MIN_DURATION  Minimum job duration in seconds to trigger notification (default: 15)
#   REMOTE_JOBS_SLACK_VERBOSE=1   Include directory and command in message
#

set -euo pipefail

if [ $# -lt 3 ]; then
    echo "Usage: $0 <session-name> <exit-code> <host> [metadata-file]"
    exit 1
fi

SESSION_NAME="$1"
EXIT_CODE="$2"
HOST="$3"
METADATA_FILE="${4:-}"

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
# Use provided metadata file, or fall back to legacy location
if [ -z "$METADATA_FILE" ]; then
    METADATA_FILE="/tmp/tmux-${SESSION_NAME}.meta"
fi
# Expand tilde if present
METADATA_FILE="${METADATA_FILE/#\~/$HOME}"

duration_text=""
working_dir=""
command=""
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
    # Extract working_dir and command for verbose mode
    working_dir=$(grep '^working_dir=' "$METADATA_FILE" | cut -d= -f2- || true)
    command=$(grep '^command=' "$METADATA_FILE" | cut -d= -f2- || true)
fi

# Get notification settings (defaults: notify all, 15s minimum duration)
NOTIFY_MODE="${REMOTE_JOBS_SLACK_NOTIFY:-all}"
MIN_DURATION="${REMOTE_JOBS_SLACK_MIN_DURATION:-15}"

# Check if we should send notification based on mode
case "$NOTIFY_MODE" in
    none)
        exit 0
        ;;
    failures)
        if [ "$EXIT_CODE" -eq 0 ]; then
            exit 0
        fi
        ;;
    all|*)
        # Send notification (default)
        ;;
esac

# Check minimum duration threshold (only if we have duration info)
if [ -n "${duration_secs:-}" ] && [ "$MIN_DURATION" -gt 0 ]; then
    if [ "$duration_secs" -lt "$MIN_DURATION" ]; then
        # Job too short, skip notification (unless it failed)
        if [ "$EXIT_CODE" -eq 0 ]; then
            exit 0
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

# Build message
message="$status_emoji Job *$SESSION_NAME* on \`$HOST\` $status_text$duration_text"

# Add verbose info if enabled
if [ "${REMOTE_JOBS_SLACK_VERBOSE:-}" = "1" ]; then
    if [ -n "$working_dir" ]; then
        message="$message\n• Directory: \`$working_dir\`"
    fi
    if [ -n "$command" ]; then
        message="$message\n• Command: \`$command\`"
    fi
fi

# Send Slack notification
curl -s -X POST -H 'Content-type: application/json' \
    --data "{\"text\":\"$message\"}" \
    "$WEBHOOK_URL" > /dev/null 2>&1 || true
