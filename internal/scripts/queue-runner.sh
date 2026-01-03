# BUILD: 12
#!/usr/bin/env bash
#
# Queue runner for remote-jobs
# This script runs on the remote host and processes jobs from a queue file.
#
# Usage:
#   queue-runner.sh <queue-name>
#
# Queue file format (one job per line, tab-separated):
#   {job_id}\t{working_dir}\t{command}\t{description}\t{env_vars_b64}\t{dependencies}
#
# env_vars_b64 is base64-encoded newline-separated VAR=value pairs (optional)
# dependencies is a comma-separated list of job IDs to wait for before starting (optional)
#   Each entry can be "ID" (requires success) or "ID:any" (waits for completion)
#
# Files:
#   ~/.cache/remote-jobs/queue/{queue-name}.queue    - Queue file (jobs waiting)
#   ~/.cache/remote-jobs/queue/{queue-name}.current  - Currently running job ID
#   ~/.cache/remote-jobs/queue/{queue-name}.runner.pid - Runner process ID
#   ~/.cache/remote-jobs/queue/runner-{queue-name}.log - Runner operations log
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
RUNNER_LOG="$QUEUE_DIR/runner-${QUEUE_NAME}.log"
NOTIFY_SCRIPT="/tmp/remote-jobs-notify-slack.sh"

# Create directories
mkdir -p "$QUEUE_DIR" "$LOG_DIR"

# Log operation to persistent runner log (JSONL format)
log_op() {
    local op="$1"
    local job_id="${2:-}"
    local detail="${3:-}"
    local ts
    ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    local entry="{\"t\":\"$ts\",\"op\":\"$op\",\"queue\":\"$QUEUE_NAME\""
    [ -n "$job_id" ] && entry="$entry,\"job\":$job_id"
    [ -n "$detail" ] && entry="$entry,\"detail\":\"$detail\""
    entry="$entry}"
    echo "$entry" >> "$RUNNER_LOG"
}

# Write PID file
echo $$ > "$PID_FILE"

# Cleanup on exit
cleanup() {
    rm -f "$PID_FILE" "$CURRENT_FILE"
}
trap cleanup EXIT

# Cross-platform file locking using mkdir (works on Linux and macOS)
# mkdir is atomic on all POSIX systems
LOCK_DIR="$QUEUE_FILE.lock.d"

acquire_lock() {
    while ! mkdir "$LOCK_DIR" 2>/dev/null; do
        sleep 0.01
    done
}

release_lock() {
    rmdir "$LOCK_DIR" 2>/dev/null || true
}

log_op "queue.start" "" "queue=$QUEUE_NAME pid=$$"
echo "Queue runner started for queue: $QUEUE_NAME"
echo "Queue file: $QUEUE_FILE"
echo "PID: $$"
echo ""

# Main loop
while true; do
    # Check for STOP signal
    if [ -f "$QUEUE_DIR/${QUEUE_NAME}.stop" ]; then
        log_op "queue.stop" "" "stop signal received"
        echo "STOP signal received, exiting after current job..."
        rm -f "$QUEUE_DIR/${QUEUE_NAME}.stop"
        break
    fi

    # Check for .start_now file - if present, promote that job to front of queue
    START_NOW_FILE="$QUEUE_DIR/${QUEUE_NAME}.start_now"
    if [ -f "$START_NOW_FILE" ]; then
        start_now_id=$(cat "$START_NOW_FILE" 2>/dev/null | tr -d '[:space:]')
        rm -f "$START_NOW_FILE"
        if [ -n "$start_now_id" ] && [ -f "$QUEUE_FILE" ]; then
            # Extract the job line and move it to front (under lock)
            acquire_lock
            job_to_promote=$(grep "^${start_now_id}	" "$QUEUE_FILE" 2>/dev/null || true)
            if [ -n "$job_to_promote" ]; then
                # Remove from current position and add to front
                temp_file=$(mktemp)
                grep -v "^${start_now_id}	" "$QUEUE_FILE" > "$temp_file" 2>/dev/null || true
                { echo "$job_to_promote"; cat "$temp_file"; } > "$QUEUE_FILE"
                rm -f "$temp_file"
                echo "Promoted job $start_now_id to front of queue"
            fi
            release_lock
        fi
    fi

    # Check if queue file exists
    if [ ! -f "$QUEUE_FILE" ]; then
        sleep 5
        continue
    fi

    # Pop first job from queue (under lock to prevent race with concurrent appends)
    acquire_lock
    job_line=$(head -n 1 "$QUEUE_FILE" 2>/dev/null || true)
    if [ -n "$job_line" ]; then
        temp_file=$(mktemp)
        tail -n +2 "$QUEUE_FILE" > "$temp_file" 2>/dev/null || true
        mv "$temp_file" "$QUEUE_FILE"
    fi
    release_lock

    if [ -z "$job_line" ]; then
        # Queue is empty, wait and check again
        sleep 5
        continue
    fi

    # Parse job line (tab-separated: job_id, working_dir, command, description, env_vars_b64, dependencies)
    # Normalize: convert literal \t to real tabs for backwards compatibility with older queue entries
    job_line=$(printf '%b' "$job_line")
    # Use awk to properly handle empty fields (bash read collapses consecutive delimiters)
    job_id=$(echo "$job_line" | awk -F'\t' '{print $1}')
    working_dir=$(echo "$job_line" | awk -F'\t' '{print $2}')
    command=$(echo "$job_line" | awk -F'\t' '{print $3}')
    description=$(echo "$job_line" | awk -F'\t' '{print $4}')
    env_vars_b64=$(echo "$job_line" | awk -F'\t' '{print $5}')
    deps_spec=$(echo "$job_line" | awk -F'\t' '{print $6}')

    if [ -z "$job_id" ] || [ -z "$command" ]; then
        echo "Invalid job line (missing job_id or command), skipping: $job_line"
        continue
    fi

    # Skip jobs that already have a status file (already completed in a previous run)
    existing_status=$(ls -t "$LOG_DIR/${job_id}"-*.status 2>/dev/null | head -1 || true)
    if [ -n "$existing_status" ]; then
        echo "Job $job_id: already completed, skipping (status file exists)"
        continue
    fi

    # Check dependencies if specified (comma-separated list of job_id[:any])
    if [ -n "$deps_spec" ]; then
        IFS=',' read -ra dep_entries <<< "$deps_spec"
        unmet_dependency=false
        skip_due_to_failure=false
        skip_reason=""

        for dep_entry in "${dep_entries[@]}"; do
            [ -z "$dep_entry" ] && continue

            dep_id="${dep_entry%%:*}"
            dep_mode="${dep_entry#*:}"
            if [ "$dep_mode" = "$dep_entry" ]; then
                dep_mode="success"
            fi

            dep_status_file=$(ls -t "$LOG_DIR/${dep_id}"-*.status 2>/dev/null | head -1 || true)
            if [ -z "$dep_status_file" ]; then
                unmet_dependency=true
                break
            fi

            dep_exit=$(cat "$dep_status_file")
            if [ "$dep_mode" != "any" ] && [ "$dep_exit" != "0" ]; then
                skip_due_to_failure=true
                skip_reason="dependency job $dep_id failed with exit code $dep_exit"
                break
            fi
        done

        if [ "$unmet_dependency" = true ]; then
            echo "Job $job_id: waiting for dependencies to complete"
            echo "$job_line" >> "$QUEUE_FILE"
            sleep 10
            continue
        fi

        if [ "$skip_due_to_failure" = true ]; then
            log_op "job.skipped" "$job_id" "$skip_reason"
            echo "Job $job_id: skipped, $skip_reason"
            timestamp=$(date +%Y%m%d-%H%M%S)
            echo "SKIPPED: $skip_reason" > "$LOG_DIR/${job_id}-${timestamp}.log"
            echo "1" > "$LOG_DIR/${job_id}-${timestamp}.status"
            continue
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

    log_op "job.start" "$job_id" "cmd=$command"
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
    # Expand tilde in working_dir (if specified)
    eval_working_dir="${working_dir/#\~/$HOME}"

    set +e
    (
        # Only cd if working directory is specified
        if [ -n "$working_dir" ]; then
            cd "$eval_working_dir" 2>/dev/null || {
                echo "ERROR: Could not cd to $working_dir" >> "$log_file"
                exit 1
            }
        fi

        # Load dotenv files for environment customization
        if [ -f ".env" ]; then
            echo "Loading .env"
            set -a
            # shellcheck disable=SC1091
            source ./.env
            set +a
        fi

        if [ -f ".env.local" ]; then
            echo "Loading .env.local"
            set -a
            # shellcheck disable=SC1091
            source ./.env.local
            set +a
        fi

        # Source .envrc if present to load environment customizations
        if [ -f ".envrc" ]; then
            echo "Loading .envrc"
            # shellcheck disable=SC1091
            source ./.envrc
        fi

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

    # Robust wait: poll for process completion instead of blocking wait
    # This handles cases where the process is killed externally and wait doesn't return
    exit_code=0
    while true; do
        # Check if process still exists
        if ! kill -0 $cmd_pid 2>/dev/null; then
            # Process is gone, get exit code via wait
            wait $cmd_pid 2>/dev/null
            exit_code=$?
            break
        fi

        # Check if status file was written (job completed via normal path)
        if [ -f "$status_file" ]; then
            exit_code=$(cat "$status_file")
            # Give process a moment to fully exit, then force cleanup
            sleep 1
            kill -0 $cmd_pid 2>/dev/null && kill $cmd_pid 2>/dev/null
            wait $cmd_pid 2>/dev/null || true
            break
        fi

        # Check for STOP signal while waiting
        if [ -f "$QUEUE_DIR/${QUEUE_NAME}.stop" ]; then
            echo "STOP signal received while job running, killing job..."
            kill $cmd_pid 2>/dev/null || true
            wait $cmd_pid 2>/dev/null
            exit_code=$?
            break
        fi

        sleep 2
    done
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
        log_op "job.completed" "$job_id" "exit=0 duration=${duration}s"
        echo "Job $job_id completed successfully in $duration_text"
    else
        log_op "job.failed" "$job_id" "exit=$exit_code duration=${duration}s"
        echo "Job $job_id failed with exit code $exit_code in $duration_text"
    fi

    # Clear current job and clean up PID file (prevents killing wrong process if PID is reused)
    rm -f "$CURRENT_FILE" "$pid_file"

    # Send Slack notification if script exists
    if [ -x "$NOTIFY_SCRIPT" ]; then
        "$NOTIFY_SCRIPT" "rj-$job_id" "$exit_code" "$(hostname)" "$meta_file" 2>/dev/null || true
    fi

    echo ""
done

log_op "queue.stop" "" "normal exit"
echo "Queue runner exiting"
