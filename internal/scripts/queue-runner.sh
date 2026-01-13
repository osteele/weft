# BUILD: 18
#!/usr/bin/env bash
#
# Queue runner for remote-jobs
# Uses append-only JSONL command log with jq for parsing.
#
# Usage:
#   queue-runner-v2.sh <queue-name>
#
# Command log format (JSONL, one command per line):
#   {"ts":"...","op":"add","job":{"id":123,"dir":"/path","cmd":"...","desc":"...","env":["..."],"deps":"..."}}
#   {"ts":"...","op":"priority","job_id":123}
#   {"ts":"...","op":"cancel","job_id":123}
#   {"ts":"...","op":"stop"}
#
# Files:
#   ~/.cache/remote-jobs/queue/{queue}.commands    - Command log (CLI appends, runner reads)
#   ~/.cache/remote-jobs/queue/{queue}.state.json  - Runner state (runner writes)
#   ~/.cache/remote-jobs/queue/{queue}.current     - Currently running job ID
#   ~/.cache/remote-jobs/queue/{queue}.runner.pid  - Runner process ID
#   ~/.cache/remote-jobs/queue/runner-{queue}.log  - Runner operations log
#   ~/.cache/remote-jobs/logs/{job_id}.log         - Job output
#   ~/.cache/remote-jobs/logs/{job_id}.status      - Exit code
#   ~/.cache/remote-jobs/logs/{job_id}.meta        - Metadata
#
# Environment Variables (for Slack notifications):
#   REMOTE_JOBS_SLACK_WEBHOOK     Slack webhook URL
#   REMOTE_JOBS_SLACK_NOTIFY      When to notify: "all" (default), "failures", "none"
#   REMOTE_JOBS_SLACK_MIN_DURATION  Minimum job duration to trigger notification
#   REMOTE_JOBS_SLACK_VERBOSE=1   Include directory and command in message
#

set -euo pipefail

# Check for jq
if ! command -v jq &>/dev/null; then
    echo "ERROR: jq is required but not installed" >&2
    exit 1
fi

QUEUE_NAME="${1:-default}"
QUEUE_DIR="$HOME/.cache/remote-jobs/queue"
LOG_DIR="$HOME/.cache/remote-jobs/logs"
COMMANDS_FILE="$QUEUE_DIR/${QUEUE_NAME}.commands"
STATE_FILE="$QUEUE_DIR/${QUEUE_NAME}.state.json"
CURRENT_FILE="$QUEUE_DIR/${QUEUE_NAME}.current"
PID_FILE="$QUEUE_DIR/${QUEUE_NAME}.runner.pid"
RUNNER_LOG="$QUEUE_DIR/runner-${QUEUE_NAME}.log"
NOTIFY_SCRIPT="/tmp/remote-jobs-notify-slack.sh"

# Create directories
mkdir -p "$QUEUE_DIR" "$LOG_DIR"

# Initialize state file if it doesn't exist
if [ ! -f "$STATE_FILE" ]; then
    echo '{"cursor":"","cursor_line":0,"pending":[],"current":null}' > "$STATE_FILE"
fi

# Log operation to persistent runner log (JSONL format)
log_op() {
    local op="$1"
    local job_id="${2:-}"
    local detail="${3:-}"
    local ts
    ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)

    # Build JSON properly with jq to handle escaping
    if [ -n "$job_id" ] && [ -n "$detail" ]; then
        jq -nc --arg t "$ts" --arg op "$op" --arg q "$QUEUE_NAME" --argjson job "$job_id" --arg detail "$detail" \
            '{t:$t,op:$op,queue:$q,job:$job,detail:$detail}' >> "$RUNNER_LOG"
    elif [ -n "$job_id" ]; then
        jq -nc --arg t "$ts" --arg op "$op" --arg q "$QUEUE_NAME" --argjson job "$job_id" \
            '{t:$t,op:$op,queue:$q,job:$job}' >> "$RUNNER_LOG"
    elif [ -n "$detail" ]; then
        jq -nc --arg t "$ts" --arg op "$op" --arg q "$QUEUE_NAME" --arg detail "$detail" \
            '{t:$t,op:$op,queue:$q,detail:$detail}' >> "$RUNNER_LOG"
    else
        jq -nc --arg t "$ts" --arg op "$op" --arg q "$QUEUE_NAME" \
            '{t:$t,op:$op,queue:$q}' >> "$RUNNER_LOG"
    fi
}

# Write PID file
echo $$ > "$PID_FILE"

# Cleanup on exit
cleanup() {
    rm -f "$PID_FILE" "$CURRENT_FILE"
}
trap cleanup EXIT

# Load state from file
load_state() {
    if [ -f "$STATE_FILE" ]; then
        STATE_CURSOR=$(jq -r '.cursor // ""' "$STATE_FILE")
        STATE_CURSOR_LINE=$(jq -r '.cursor_line // 0' "$STATE_FILE")
        # Load pending as newline-separated list
        STATE_PENDING=$(jq -r '.pending[]' "$STATE_FILE" 2>/dev/null || true)
    else
        STATE_CURSOR=""
        STATE_CURSOR_LINE=0
        STATE_PENDING=""
    fi
}

# Save state to file
save_state() {
    local current_json="null"
    if [ -n "${CURRENT_JOB_ID:-}" ]; then
        current_json="$CURRENT_JOB_ID"
    fi

    # Convert pending list to JSON array
    local pending_json="[]"
    if [ -n "$STATE_PENDING" ]; then
        pending_json=$(echo "$STATE_PENDING" | jq -Rs 'split("\n") | map(select(. != "") | tonumber)')
    fi

    jq -nc \
        --arg cursor "$STATE_CURSOR" \
        --argjson cursor_line "$STATE_CURSOR_LINE" \
        --argjson pending "$pending_json" \
        --argjson current "$current_json" \
        '{cursor:$cursor,cursor_line:$cursor_line,pending:$pending,current:$current}' > "$STATE_FILE"
}

# Add job to pending list (at end)
add_pending() {
    local job_id="$1"
    if [ -z "$STATE_PENDING" ]; then
        STATE_PENDING="$job_id"
    else
        STATE_PENDING="$STATE_PENDING"$'\n'"$job_id"
    fi
}

# Add job to front of pending list
priority_pending() {
    local job_id="$1"
    # Remove if already present
    STATE_PENDING=$(echo "$STATE_PENDING" | grep -v "^${job_id}$" || true)
    # Add to front
    if [ -z "$STATE_PENDING" ]; then
        STATE_PENDING="$job_id"
    else
        STATE_PENDING="$job_id"$'\n'"$STATE_PENDING"
    fi
}

# Remove job from pending list
remove_pending() {
    local job_id="$1"
    STATE_PENDING=$(echo "$STATE_PENDING" | grep -v "^${job_id}$" || true)
}

# Get and remove first job from pending list
# Sets POPPED_JOB global variable (don't use command substitution - it runs in subshell)
pop_pending() {
    POPPED_JOB=$(echo "$STATE_PENDING" | head -1)
    STATE_PENDING=$(echo "$STATE_PENDING" | tail -n +2)
}

# Check if pending list is empty
pending_empty() {
    [ -z "$STATE_PENDING" ]
}

# Process new commands from the command log
process_commands() {
    [ ! -f "$COMMANDS_FILE" ] && return

    local line_num=0
    local stop_requested=false

    while IFS= read -r line; do
        line_num=$((line_num + 1))

        # Skip lines we've already processed
        if [ "$line_num" -le "$STATE_CURSOR_LINE" ]; then
            continue
        fi

        # Skip empty lines
        [ -z "$line" ] && continue

        local op ts
        op=$(echo "$line" | jq -r '.op // ""')
        ts=$(echo "$line" | jq -r '.ts // ""')

        case "$op" in
            add)
                local job_id
                job_id=$(echo "$line" | jq -r '.job.id')
                add_pending "$job_id"
                # Store job data for later use
                echo "$line" | jq -c '.job' > "$QUEUE_DIR/job-${job_id}.json"
                log_op "cmd.add" "$job_id"
                echo "Command: add job $job_id"
                ;;
            priority)
                local job_id
                job_id=$(echo "$line" | jq -r '.job_id')
                priority_pending "$job_id"
                log_op "cmd.priority" "$job_id"
                echo "Command: prioritize job $job_id"
                ;;
            cancel)
                local job_id
                job_id=$(echo "$line" | jq -r '.job_id')
                remove_pending "$job_id"
                rm -f "$QUEUE_DIR/job-${job_id}.json"
                log_op "cmd.cancel" "$job_id"
                echo "Command: cancel job $job_id"
                ;;
            stop)
                stop_requested=true
                log_op "cmd.stop"
                echo "Command: stop requested"
                ;;
            *)
                echo "Unknown command: $op"
                ;;
        esac

        # Update cursor
        STATE_CURSOR="$ts"
        STATE_CURSOR_LINE="$line_num"
    done < "$COMMANDS_FILE"

    save_state

    if [ "$stop_requested" = true ]; then
        return 1
    fi
    return 0
}

# Get job data from stored file
get_job_data() {
    local job_id="$1"
    local job_file="$QUEUE_DIR/job-${job_id}.json"
    if [ -f "$job_file" ]; then
        cat "$job_file"
    else
        echo "{}"
    fi
}

# Check if a job is already completed
job_completed() {
    local job_id="$1"
    [ -f "$LOG_DIR/${job_id}.status" ] && return 0
    ls "$LOG_DIR/${job_id}"-*.status &>/dev/null 2>&1 && return 0
    return 1
}

# Check if a job is currently running
job_running() {
    local job_id="$1"
    local pid_file="$LOG_DIR/${job_id}.pid"

    if [ -f "$pid_file" ]; then
        local pid
        pid=$(cat "$pid_file" 2>/dev/null | tail -1)
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            return 0
        fi
    fi

    # Check archived pid files
    local archived_pid_file
    archived_pid_file=$(ls -t "$LOG_DIR/${job_id}"-*.pid 2>/dev/null | head -1 || true)
    if [ -n "$archived_pid_file" ]; then
        local pid
        pid=$(cat "$archived_pid_file" 2>/dev/null | tail -1)
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            return 0
        fi
    fi

    return 1
}

# Check dependencies for a job
check_dependencies() {
    local deps_spec="$1"
    [ -z "$deps_spec" ] && return 0

    IFS=',' read -ra dep_entries <<< "$deps_spec"

    for dep_entry in "${dep_entries[@]}"; do
        [ -z "$dep_entry" ] && continue

        local dep_id="${dep_entry%%:*}"
        local dep_mode="${dep_entry#*:}"
        [ "$dep_mode" = "$dep_entry" ] && dep_mode="success"

        # Find status file
        local dep_status_file=""
        if [ -f "$LOG_DIR/${dep_id}.status" ]; then
            dep_status_file="$LOG_DIR/${dep_id}.status"
        else
            dep_status_file=$(ls -t "$LOG_DIR/${dep_id}"-*.status 2>/dev/null | head -1 || true)
        fi

        if [ -z "$dep_status_file" ]; then
            echo "waiting"  # Dependency not completed yet
            return 0
        fi

        local dep_exit
        dep_exit=$(cat "$dep_status_file")
        if [ "$dep_mode" != "any" ] && [ "$dep_exit" != "0" ]; then
            echo "failed:$dep_id:$dep_exit"  # Dependency failed
            return 0
        fi
    done

    echo "ok"
}

# Run a single job
run_job() {
    local job_id="$1"
    local job_data
    job_data=$(get_job_data "$job_id")

    # Extract job fields using jq
    local working_dir command description env_vars deps_spec
    working_dir=$(echo "$job_data" | jq -r '.dir // ""')
    command=$(echo "$job_data" | jq -r '.cmd // ""')
    description=$(echo "$job_data" | jq -r '.desc // ""')
    deps_spec=$(echo "$job_data" | jq -r '.deps // ""')

    if [ -z "$command" ]; then
        echo "Job $job_id: no command found, skipping"
        return 1
    fi

    # Check dependencies
    local dep_result
    dep_result=$(check_dependencies "$deps_spec")
    case "$dep_result" in
        waiting)
            echo "Job $job_id: waiting for dependencies"
            return 2  # Re-queue
            ;;
        failed:*)
            local dep_info="${dep_result#failed:}"
            log_op "job.skipped" "$job_id" "dependency $dep_info failed"
            echo "Job $job_id: skipped, dependency $dep_info failed"
            echo "SKIPPED: dependency $dep_info failed" > "$LOG_DIR/${job_id}.log"
            echo "1" > "$LOG_DIR/${job_id}.status"
            rm -f "$QUEUE_DIR/job-${job_id}.json"
            return 0
            ;;
    esac

    # Skip if already completed or running
    if job_completed "$job_id"; then
        echo "Job $job_id: already completed, skipping"
        rm -f "$QUEUE_DIR/job-${job_id}.json"
        return 0
    fi

    if job_running "$job_id"; then
        echo "Job $job_id: already running, skipping"
        return 0
    fi

    local start_time
    start_time=$(date +%s)

    # File paths
    local log_file="$LOG_DIR/${job_id}.log"
    local status_file="$LOG_DIR/${job_id}.status"
    local meta_file="$LOG_DIR/${job_id}.meta"
    local pid_file="$LOG_DIR/${job_id}.pid"

    # Archive any existing files from previous runs
    for ext in log status meta pid; do
        local f="$LOG_DIR/${job_id}.$ext"
        if [ -f "$f" ]; then
            local mtime ts
            mtime=$(stat -c %Y "$f" 2>/dev/null || stat -f %m "$f" 2>/dev/null)
            if [ -n "$mtime" ]; then
                ts=$(date -r "$mtime" "+%Y%m%d-%H%M%S" 2>/dev/null || date -d "@$mtime" "+%Y%m%d-%H%M%S" 2>/dev/null)
                [ -n "$ts" ] && mv "$f" "$LOG_DIR/${job_id}-${ts}.$ext"
            fi
        fi
    done

    # Mark as current
    echo "$job_id" > "$CURRENT_FILE"
    CURRENT_JOB_ID="$job_id"
    save_state

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

    # Write log header
    {
        echo "=== START $(date) ==="
        echo "job_id: $job_id"
        echo "cd: $working_dir"
        echo "cmd: $command"
        echo "==="
    } > "$log_file"

    # Expand tilde in working_dir
    local eval_working_dir="${working_dir/#\~/$HOME}"

    # Get env vars as array
    local env_vars_json
    env_vars_json=$(echo "$job_data" | jq -r '.env // []')

    set +e
    (
        # cd if working directory specified
        if [ -n "$working_dir" ]; then
            cd "$eval_working_dir" 2>/dev/null || {
                echo "ERROR: Could not cd to $working_dir" >> "$log_file"
                exit 1
            }
        fi

        # Load dotenv files
        [ -f ".env" ] && { echo "Loading .env"; set -a; source ./.env; set +a; }
        [ -f ".env.local" ] && { echo "Loading .env.local"; set -a; source ./.env.local; set +a; }
        [ -f ".envrc" ] && { echo "Loading .envrc"; source ./.envrc; }

        # Apply environment variables from job
        while IFS= read -r env_line; do
            [ -n "$env_line" ] && [ "$env_line" != "null" ] && export "$env_line"
        done < <(echo "$env_vars_json" | jq -r '.[]' 2>/dev/null)

        # Record PID and exec
        echo $BASHPID > "$pid_file"
        exec bash -c "$command"
    ) >> "$log_file" 2>&1 &
    local cmd_pid=$!

    # Wait for completion
    local exit_code=0
    while true; do
        if ! kill -0 $cmd_pid 2>/dev/null; then
            wait $cmd_pid 2>/dev/null
            exit_code=$?
            break
        fi

        if [ -f "$status_file" ]; then
            exit_code=$(cat "$status_file")
            sleep 1
            kill -0 $cmd_pid 2>/dev/null && kill $cmd_pid 2>/dev/null
            wait $cmd_pid 2>/dev/null || true
            break
        fi

        # Check for stop command (re-process commands)
        if ! process_commands 2>/dev/null; then
            echo "Stop requested while job running, killing job..."
            kill $cmd_pid 2>/dev/null || true
            wait $cmd_pid 2>/dev/null
            exit_code=$?
            break
        fi

        sleep 2
    done
    set -e

    local end_time duration
    end_time=$(date +%s)
    duration=$((end_time - start_time))

    # Write status
    echo "$exit_code" > "$status_file"
    echo "=== END exit=$exit_code $(date) ===" >> "$log_file"

    # Format duration
    local hours minutes seconds duration_text
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

    # Cleanup
    rm -f "$CURRENT_FILE" "$pid_file" "$QUEUE_DIR/job-${job_id}.json"
    CURRENT_JOB_ID=""
    save_state

    # Slack notification
    if [ -x "$NOTIFY_SCRIPT" ]; then
        "$NOTIFY_SCRIPT" "rj-$job_id" "$exit_code" "$(hostname)" "$meta_file" 2>/dev/null || true
    fi

    return 0
}

# Main
log_op "queue.start" "" "queue=$QUEUE_NAME pid=$$"
echo "Queue runner v2 started for queue: $QUEUE_NAME"
echo "Commands file: $COMMANDS_FILE"
echo "State file: $STATE_FILE"
echo "PID: $$"
echo ""

load_state

# Main loop
while true; do
    # Process any new commands
    if ! process_commands; then
        log_op "queue.stop" "" "stop command received"
        echo "Stop command received, exiting..."
        break
    fi

    # Check if we have pending jobs
    if pending_empty; then
        sleep 5
        continue
    fi

    # Get next job (pop_pending sets POPPED_JOB global, can't use $() - subshell loses state)
    pop_pending

    if [ -z "$POPPED_JOB" ]; then
        sleep 5
        continue
    fi

    # Run the job
    run_result=0
    run_job "$POPPED_JOB" || run_result=$?

    case "$run_result" in
        2)
            # Re-queue (dependency waiting)
            add_pending "$POPPED_JOB"
            save_state
            sleep 10
            ;;
        *)
            # For any other result (success or failure), save state to reflect the job was removed from pending
            save_state
            ;;
    esac
done

log_op "queue.stop" "" "normal exit"
echo "Queue runner exiting"
