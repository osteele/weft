#!/usr/bin/env bash
#
# Migration script for remote-jobs queue runner v2
# Converts from old TSV queue format to new JSONL command log format.
#
# Usage:
#   migrate-queue-v2.sh [queue-name]
#
# This script:
#   1. Stops the queue runner if running
#   2. Converts existing .queue file to .commands file
#   3. Initializes .state.json
#   4. Backs up the old .queue file
#
# Prerequisites:
#   - jq must be installed
#   - No jobs should be running (script will warn if any are)
#

set -euo pipefail

QUEUE_NAME="${1:-default}"
QUEUE_DIR="$HOME/.cache/remote-jobs/queue"
LOG_DIR="$HOME/.cache/remote-jobs/logs"

OLD_QUEUE_FILE="$QUEUE_DIR/${QUEUE_NAME}.queue"
NEW_COMMANDS_FILE="$QUEUE_DIR/${QUEUE_NAME}.commands"
STATE_FILE="$QUEUE_DIR/${QUEUE_NAME}.state.json"
PID_FILE="$QUEUE_DIR/${QUEUE_NAME}.runner.pid"
CURRENT_FILE="$QUEUE_DIR/${QUEUE_NAME}.current"

echo "=== Queue Migration to v2 ==="
echo "Queue: $QUEUE_NAME"
echo ""

# Check for jq
if ! command -v jq &>/dev/null; then
    echo "ERROR: jq is required but not installed" >&2
    exit 1
fi

# Check if a job is currently running
if [ -f "$CURRENT_FILE" ]; then
    current_job=$(cat "$CURRENT_FILE")
    echo "WARNING: Job $current_job appears to be running."
    echo "Please wait for it to complete or kill it before migrating."
    read -p "Continue anyway? [y/N] " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        exit 1
    fi
fi

# Stop queue runner if running
if [ -f "$PID_FILE" ]; then
    pid=$(cat "$PID_FILE")
    if kill -0 "$pid" 2>/dev/null; then
        echo "Stopping queue runner (PID $pid)..."
        kill "$pid" 2>/dev/null || true
        sleep 2
        if kill -0 "$pid" 2>/dev/null; then
            echo "Force killing queue runner..."
            kill -9 "$pid" 2>/dev/null || true
        fi
    fi
    rm -f "$PID_FILE"
fi

# Check if old queue file exists
if [ ! -f "$OLD_QUEUE_FILE" ]; then
    echo "No existing queue file found at $OLD_QUEUE_FILE"
    echo "Creating empty commands file and state..."

    # Create empty commands file
    touch "$NEW_COMMANDS_FILE"

    # Create initial state
    echo '{"cursor":"","cursor_line":0,"pending":[],"current":null}' > "$STATE_FILE"

    echo "Migration complete (no jobs to migrate)."
    exit 0
fi

# Count jobs in old queue
job_count=$(wc -l < "$OLD_QUEUE_FILE" | tr -d ' ')
echo "Found $job_count job(s) in old queue file."

# Backup old queue file
backup_file="$OLD_QUEUE_FILE.backup.$(date +%Y%m%d-%H%M%S)"
cp "$OLD_QUEUE_FILE" "$backup_file"
echo "Backed up old queue to: $backup_file"

# Convert each job to a command
echo "Converting jobs to commands..."
line_num=0
converted=0

while IFS= read -r line; do
    line_num=$((line_num + 1))

    # Skip empty lines
    [ -z "$line" ] && continue

    # Parse old TSV format: job_id, working_dir, command, description, env_vars_b64, dependencies
    # The command field may contain escaped \n and \t sequences
    job_id=$(echo "$line" | awk -F'\t' '{print $1}')
    working_dir=$(echo "$line" | awk -F'\t' '{print $2}')
    command_raw=$(echo "$line" | awk -F'\t' '{print $3}')
    description_raw=$(echo "$line" | awk -F'\t' '{print $4}')
    env_vars_b64=$(echo "$line" | awk -F'\t' '{print $5}')
    deps_spec=$(echo "$line" | awk -F'\t' '{print $6}')

    # Convert escape sequences in command and description
    command=$(printf '%b' "$command_raw")
    description=$(printf '%b' "$description_raw")

    # Decode env vars from base64 to array
    env_array="[]"
    if [ -n "$env_vars_b64" ]; then
        # Decode and convert to JSON array
        env_decoded=$(echo "$env_vars_b64" | base64 -d 2>/dev/null || true)
        if [ -n "$env_decoded" ]; then
            env_array=$(echo "$env_decoded" | jq -Rs 'split("\n") | map(select(. != ""))')
        fi
    fi

    # Skip if job_id is empty or not a number
    if [ -z "$job_id" ] || ! [[ "$job_id" =~ ^[0-9]+$ ]]; then
        echo "  Skipping invalid line $line_num: $line"
        continue
    fi

    # Generate timestamp for this command
    ts=$(date -u +%Y-%m-%dT%H:%M:%S.%N)Z
    # Truncate nanoseconds to microseconds for compatibility
    ts="${ts:0:26}Z"

    # Build the add command JSON
    jq -nc \
        --arg ts "$ts" \
        --argjson id "$job_id" \
        --arg dir "$working_dir" \
        --arg cmd "$command" \
        --arg desc "$description" \
        --argjson env "$env_array" \
        --arg deps "$deps_spec" \
        '{ts:$ts,op:"add",job:{id:$id,dir:$dir,cmd:$cmd,desc:$desc,env:$env,deps:$deps}}' >> "$NEW_COMMANDS_FILE"

    converted=$((converted + 1))
    echo "  Converted job $job_id"

done < "$OLD_QUEUE_FILE"

echo ""
echo "Converted $converted job(s) to commands file."

# Create initial state with cursor at beginning (so runner processes all commands)
echo '{"cursor":"","cursor_line":0,"pending":[],"current":null}' > "$STATE_FILE"
echo "Created initial state file."

# Remove old queue file (backup was made)
rm -f "$OLD_QUEUE_FILE"
echo "Removed old queue file."

# Clean up old lock directory if present
rm -rf "$OLD_QUEUE_FILE.lock.d" 2>/dev/null || true

echo ""
echo "=== Migration Complete ==="
echo ""
echo "Files created:"
echo "  Commands: $NEW_COMMANDS_FILE"
echo "  State:    $STATE_FILE"
echo "  Backup:   $backup_file"
echo ""
echo "To start the new queue runner:"
echo "  bash ~/.cache/remote-jobs/scripts/queue-runner-v2.sh $QUEUE_NAME"
echo ""
echo "Or via remote-jobs CLI (after updating to new version):"
echo "  remote-jobs queue start $QUEUE_NAME"
