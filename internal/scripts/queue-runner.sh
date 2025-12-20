#!/bin/bash
#
# Queue runner for remote-jobs
# This script runs on the remote host and processes jobs from a queue file.
#
# Usage:
#   queue-runner.sh <queue-name>
#
# Queue file format (one job per line, tab-separated):
#   {job_id}\t{working_dir}\t{command}\t{description}\t{env_vars_b64}\t{after_job_id}
#
# env_vars_b64 is base64-encoded newline-separated VAR=value pairs (optional)
# after_job_id is the job ID to wait for before starting (optional)
#   Format: "ID" (wait for success) or "ID:any" (wait for completion)
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

    # Parse job line (tab-separated: job_id, working_dir, command, description, env_vars_b64, after_job_id)
    IFS=$'\t' read -r job_id working_dir command description env_vars_b64 after_job_id <<< "$job_line"

    if [ -z "$job_id" ] || [ -z "$working_dir" ] || [ -z "$command" ]; then
        echo "Invalid job line, skipping: $job_line"
        continue
    fi

    # Check dependency if specified
    if [ -n "$after_job_id" ]; then
        # Parse after_job_id - format is "ID" or "ID:any"
        dep_id="${after_job_id%%:*}"
        dep_mode="${after_job_id#*:}"
        if [ "$dep_mode" = "$after_job_id" ]; then
            dep_mode="success"  # Default: only run on success
        fi

        # Find status file for dependency job (use most recent)
        dep_status_file=$(ls -t "$LOG_DIR/${dep_id}"-*.status 2>/dev/null | head -1)

        if [ -z "$dep_status_file" ]; then
            # Dependency job not completed yet - put job back in queue
            echo "Job $job_id: waiting for job $dep_id to complete (not finished yet)"
            echo "$job_line" >> "$QUEUE_FILE"
            sleep 10  # Avoid busy loop
            continue
        fi

        dep_exit=$(cat "$dep_status_file")
        if [ "$dep_mode" = "success" ] && [ "$dep_exit" != "0" ]; then
            echo "Job $job_id: skipped, dependency job $dep_id failed with exit code $dep_exit"
            # Write failure status for this job
            timestamp=$(date +%Y%m%d-%H%M%S)
            echo "SKIPPED: dependency job $dep_id failed with exit code $dep_exit" > "$LOG_DIR/${job_id}-${timestamp}.log"
            echo "1" > "$LOG_DIR/${job_id}-${timestamp}.status"
            continue
        fi

        if [ "$dep_mode" = "any" ]; then
            echo "Job $job_id: dependency job $dep_id completed (exit $dep_exit), proceeding"
        else
            echo "Job $job_id: dependency job $dep_id completed successfully, proceeding"
        fi
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
        if [ -n "$env_vars_b64" ]; then
            echo "env: $(echo "$env_vars_b64" | base64 -d 2>/dev/null | tr '\n' ' ')"
        fi
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

        # Apply environment variables if present (base64 encoded, newline-separated)
        if [ -n "$env_vars_b64" ]; then
            while IFS= read -r env_line; do
                [ -n "$env_line" ] && export "$env_line"
            done < <(echo "$env_vars_b64" | base64 -d 2>/dev/null)
        fi

        # Record PID before exec - after exec, this becomes the command's PID
        echo $BASHPID > "$pid_file"
        # Use exec to replace this subshell with the actual command process
        # This ensures the recorded PID is the job process, not a wrapper
        exec bash -c "$command"
    ) >> "$log_file" 2>&1 &
    cmd_pid=$!
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
