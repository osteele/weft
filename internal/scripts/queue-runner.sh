#!/bin/bash
#
# Queue runner for remote-jobs
# This script runs on the remote host and processes jobs from a queue file.
#
# Usage:
#   queue-runner.sh <queue-name>
#
# Queue file format (one job per line, tab-separated):
#   {job_id}\t{working_dir}\t{command}\t{description}
#
# Files:
#   ~/.cache/remote-jobs/queue/{queue-name}.queue    - Queue file (jobs waiting)
#   ~/.cache/remote-jobs/queue/{queue-name}.current  - Currently running job ID
#   ~/.cache/remote-jobs/queue/{queue-name}.runner.pid - Runner process ID
#   ~/.cache/remote-jobs/logs/{job_id}-{ts}.log      - Job output
#   ~/.cache/remote-jobs/logs/{job_id}-{ts}.status   - Exit code
#   ~/.cache/remote-jobs/logs/{job_id}-{ts}.meta     - Metadata
#
# Environment Variables (for Slack notifications):
#   REMOTE_JOBS_SLACK_WEBHOOK     Slack webhook URL
#   REMOTE_JOBS_SLACK_NOTIFY      When to notify: "all" (default), "failures", "none"
#   REMOTE_JOBS_SLACK_MIN_DURATION  Minimum job duration to trigger notification
#   REMOTE_JOBS_SLACK_VERBOSE=1   Include directory and command in message
#

set -euo pipefail

QUEUE_NAME="${1:-default}"
QUEUE_DIR="$HOME/.cache/remote-jobs/queue"
LOG_DIR="$HOME/.cache/remote-jobs/logs"
QUEUE_FILE="$QUEUE_DIR/${QUEUE_NAME}.queue"
CURRENT_FILE="$QUEUE_DIR/${QUEUE_NAME}.current"
PID_FILE="$QUEUE_DIR/${QUEUE_NAME}.runner.pid"
NOTIFY_SCRIPT="/tmp/remote-jobs-notify-slack.sh"

# Create directories
mkdir -p "$QUEUE_DIR" "$LOG_DIR"

# Write PID file
echo $$ > "$PID_FILE"

# Cleanup on exit
cleanup() {
    rm -f "$PID_FILE" "$CURRENT_FILE"
}
trap cleanup EXIT

echo "Queue runner started for queue: $QUEUE_NAME"
echo "Queue file: $QUEUE_FILE"
echo "PID: $$"
echo ""

# Main loop
while true; do
    # Check for STOP signal
    if [ -f "$QUEUE_DIR/${QUEUE_NAME}.stop" ]; then
        echo "STOP signal received, exiting after current job..."
        rm -f "$QUEUE_DIR/${QUEUE_NAME}.stop"
        break
    fi

    # Check if queue file exists
    if [ ! -f "$QUEUE_FILE" ]; then
        sleep 5
        continue
    fi

    # Read first line from queue (atomic read and remove)
    job_line=$(head -n 1 "$QUEUE_FILE" 2>/dev/null || true)

    if [ -z "$job_line" ]; then
        # Queue is empty, wait and check again
        sleep 5
        continue
    fi

    # Remove first line from queue file (atomic operation)
    temp_file=$(mktemp)
    tail -n +2 "$QUEUE_FILE" > "$temp_file" 2>/dev/null || true
    mv "$temp_file" "$QUEUE_FILE"

    # Parse job line (tab-separated: job_id, working_dir, command, description)
    IFS=$'\t' read -r job_id working_dir command description <<< "$job_line"

    if [ -z "$job_id" ] || [ -z "$working_dir" ] || [ -z "$command" ]; then
        echo "Invalid job line, skipping: $job_line"
        continue
    fi

    # Generate timestamp for file names
    timestamp=$(date +%Y%m%d-%H%M%S)
    start_time=$(date +%s)

    # File paths
    log_file="$LOG_DIR/${job_id}-${timestamp}.log"
    status_file="$LOG_DIR/${job_id}-${timestamp}.status"
    meta_file="$LOG_DIR/${job_id}-${timestamp}.meta"
    pid_file="$LOG_DIR/${job_id}-${timestamp}.pid"

    # Write current job ID
    echo "$job_id" > "$CURRENT_FILE"

    echo "=========================================="
    echo "Starting job $job_id"
    echo "  Working dir: $working_dir"
    echo "  Command: $command"
    [ -n "$description" ] && echo "  Description: $description"
    echo "  Log: $log_file"
    echo "=========================================="

    # Write metadata
    {
        echo "job_id=$job_id"
        echo "working_dir=$working_dir"
        echo "command=$command"
        echo "start_time=$start_time"
        echo "host=$(hostname)"
        [ -n "$description" ] && echo "description=$description"
        echo "queue=$QUEUE_NAME"
    } > "$meta_file"

    # Run the job
    {
        echo "=== START $(date) ==="
        echo "job_id: $job_id"
        echo "cd: $working_dir"
        echo "cmd: $command"
        echo "==="
    } > "$log_file"

    # Execute command, capture exit code
    # Expand tilde in working_dir
    eval_working_dir="${working_dir/#\~/$HOME}"

    set +e
    (
        cd "$eval_working_dir" 2>/dev/null || {
            echo "ERROR: Could not cd to $working_dir" >> "$log_file"
            exit 1
        }
        eval "$command"
    ) >> "$log_file" 2>&1 &
    cmd_pid=$!
    echo $cmd_pid > "$pid_file"
    wait $cmd_pid
    exit_code=$?
    set -e

    end_time=$(date +%s)
    duration=$((end_time - start_time))

    # Write status and end marker
    echo "$exit_code" > "$status_file"
    echo "=== END exit=$exit_code $(date) ===" >> "$log_file"

    # Format duration
    hours=$((duration / 3600))
    minutes=$(((duration % 3600) / 60))
    seconds=$((duration % 60))
    if [ $hours -gt 0 ]; then
        duration_text="${hours}h ${minutes}m ${seconds}s"
    elif [ $minutes -gt 0 ]; then
        duration_text="${minutes}m ${seconds}s"
    else
        duration_text="${seconds}s"
    fi

    if [ "$exit_code" -eq 0 ]; then
        echo "Job $job_id completed successfully in $duration_text"
    else
        echo "Job $job_id failed with exit code $exit_code in $duration_text"
    fi

    # Clear current job
    rm -f "$CURRENT_FILE"

    # Send Slack notification if script exists
    if [ -x "$NOTIFY_SCRIPT" ]; then
        "$NOTIFY_SCRIPT" "rj-$job_id" "$exit_code" "$(hostname)" "$meta_file" 2>/dev/null || true
    fi

    echo ""
done

echo "Queue runner exiting"
