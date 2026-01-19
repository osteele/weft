#!/usr/bin/env bash
# BUILD: 35
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
#   ~/.cache/remote-jobs/logs/{job_id}.samples     - CPU samples (epoch + % of total cores)
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

# Concurrency and allotment tuning defaults
HOST_UTILIZATION_TARGET=80
DEFAULT_ALLOTMENT_CORES=7
WARMUP_DURATION=120
SAMPLE_INTERVAL=15
SAMPLE_WINDOW=60
HYSTERESIS_WINDOW=5
HYSTERESIS_THRESHOLD=3
INCREASE_STEP=10
DECAY_STEP=10
MIN_ALLOTMENT=10
MAX_ALLOTMENT=100

SAMPLE_COUNT=$((SAMPLE_WINDOW / SAMPLE_INTERVAL))

CPU_COUNT=$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 1)
CPU_COUNT=${CPU_COUNT:-1}
DEFAULT_ALLOTMENT=$(awk -v cores="$DEFAULT_ALLOTMENT_CORES" -v cpus="$CPU_COUNT" 'BEGIN { if (cpus <= 0) { print 60; exit } pct = (cores * 100.0) / cpus; if (pct < 1) pct = 1; if (pct > 100) pct = 100; printf "%.0f", pct }')

# CPU history: learned CPU usage by command signature
CPU_HISTORY_FILE="$QUEUE_DIR/cpu-history.json"
if [ ! -f "$CPU_HISTORY_FILE" ]; then
    echo '{}' > "$CPU_HISTORY_FILE"
fi

# Extract a signature from a command for CPU history lookup.
# Extracts script name (*.py, *.sh) or first recognizable binary.
extract_command_signature() {
    local cmd="$1"
    # Try to find a Python/shell script name
    local script
    script=$(echo "$cmd" | grep -oE '[a-zA-Z0-9_/-]+\.(py|sh)' | head -1)
    if [ -n "$script" ]; then
        # Return just the basename
        basename "$script"
        return
    fi
    # Fall back to first word after common prefixes (cd, &&, uv run, python, etc.)
    local simplified
    simplified=$(echo "$cmd" | sed -E 's/^(cd [^&]+&& *|uv run |python[0-9]* |bash -c )+//')
    echo "$simplified" | awk '{print $1}' | head -c 50
}

# Get historical CPU usage for a command signature (returns empty if unknown)
get_cpu_history() {
    local sig="$1"
    [ -z "$sig" ] && return
    jq -r --arg sig "$sig" '.[$sig].avg // empty' "$CPU_HISTORY_FILE" 2>/dev/null
}

# Update CPU history with observed usage (exponential moving average)
update_cpu_history() {
    local sig="$1"
    local observed_cpu="$2"
    [ -z "$sig" ] || [ -z "$observed_cpu" ] && return

    local current_avg current_count new_avg new_count
    current_avg=$(jq -r --arg sig "$sig" '.[$sig].avg // 0' "$CPU_HISTORY_FILE" 2>/dev/null)
    current_count=$(jq -r --arg sig "$sig" '.[$sig].count // 0' "$CPU_HISTORY_FILE" 2>/dev/null)

    # Exponential moving average with alpha=0.3 for recent bias, or simple avg if few samples
    if [ "$current_count" -lt 3 ]; then
        new_avg=$(awk -v old="$current_avg" -v new="$observed_cpu" -v n="$current_count" \
            'BEGIN { printf "%.0f", (old * n + new) / (n + 1) }')
    else
        new_avg=$(awk -v old="$current_avg" -v new="$observed_cpu" \
            'BEGIN { printf "%.0f", old * 0.7 + new * 0.3 }')
    fi
    new_count=$((current_count + 1))

    # Update the history file
    jq --arg sig "$sig" --argjson avg "$new_avg" --argjson count "$new_count" \
        '.[$sig] = {avg: $avg, count: $count}' "$CPU_HISTORY_FILE" > "${CPU_HISTORY_FILE}.tmp" \
        && mv "${CPU_HISTORY_FILE}.tmp" "$CPU_HISTORY_FILE"

    log_op "cpu_history.update" "" "sig=$sig avg=$new_avg count=$new_count observed=$observed_cpu"
}

# Running job state (JSON object keyed by job ID)
RUNNING_JSON="{}"

CURRENT_JOB_ID=""
LAST_SAMPLE_TIME=0
STOP_REQUESTED=false

# Create directories
mkdir -p "$QUEUE_DIR" "$LOG_DIR"

# Initialize state file if it doesn't exist or is empty
if [ ! -f "$STATE_FILE" ] || [ ! -s "$STATE_FILE" ]; then
    echo '{"cursor":"","cursor_line":0,"pending":[],"current":null,"running":{}}' > "$STATE_FILE"
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
    STATE_CURSOR=""
    STATE_CURSOR_LINE=0
    STATE_PENDING=""
    CURRENT_JOB_ID=""
    RUNNING_JSON="{}"

    if [ -f "$STATE_FILE" ]; then
        STATE_CURSOR=$(jq -r '.cursor // ""' "$STATE_FILE")
        STATE_CURSOR_LINE=$(jq -r '.cursor_line // 0' "$STATE_FILE")
        STATE_PENDING=$(jq -r '.pending[]' "$STATE_FILE" 2>/dev/null || true)
        CURRENT_JOB_ID=$(jq -r '.current // ""' "$STATE_FILE")
        if [ "$CURRENT_JOB_ID" = "null" ]; then
            CURRENT_JOB_ID=""
        fi

        RUNNING_JSON=$(jq -c '.running // {}' "$STATE_FILE")
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
        --argjson running "$RUNNING_JSON" \
        '{cursor:$cursor,cursor_line:$cursor_line,pending:$pending,current:$current,running:$running}' > "$STATE_FILE"
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

running_contains() {
    local job_id="$1"
    jq -e --arg id "$job_id" '.[$id] != null' <<< "$RUNNING_JSON" >/dev/null 2>&1
}

remove_running_job() {
    local job_id="$1"
    RUNNING_JSON=$(jq -c --arg id "$job_id" 'del(.[$id])' <<< "$RUNNING_JSON")
}

update_current_from_running() {
    local current
    current=$(jq -r 'if length == 0 then "" else (to_entries | max_by(.value.started_at // 0) | .key) end' <<< "$RUNNING_JSON")
    if [ -z "$current" ] || [ "$current" = "null" ]; then
        CURRENT_JOB_ID=""
        rm -f "$CURRENT_FILE"
        return
    fi
    CURRENT_JOB_ID="$current"
    echo "$CURRENT_JOB_ID" > "$CURRENT_FILE"
}

job_allotment_from_data() {
    local job_id="$1"
    local job_data
    job_data=$(get_job_data "$job_id")

    # 1. Check if CPU is explicitly specified in job data
    local cpu
    cpu=$(echo "$job_data" | jq -r '.cpu // empty')
    if [[ "$cpu" =~ ^[0-9]+$ ]]; then
        echo "$cpu"
        return
    fi

    # 2. Check CPU history for similar commands
    local cmd sig historical_cpu
    cmd=$(echo "$job_data" | jq -r '.cmd // ""')
    sig=$(extract_command_signature "$cmd")
    if [ -n "$sig" ]; then
        historical_cpu=$(get_cpu_history "$sig")
        if [ -n "$historical_cpu" ] && [ "$historical_cpu" -gt 0 ]; then
            echo "$historical_cpu"
            return
        fi
    fi

    # 3. Fall back to default
    echo "$DEFAULT_ALLOTMENT"
}

refresh_running_allotment() {
    local job_id="$1"
    local new_allotment
    new_allotment=$(job_allotment_from_data "$job_id")
    RUNNING_JSON=$(jq -c \
        --arg id "$job_id" \
        --argjson allot "$new_allotment" \
        '.[$id].local_allotment = $allot | .[$id].samples = [] | .[$id].over_hist = [] | .[$id].under_hist = []' <<< "$RUNNING_JSON")
    log_op "job.allotment_update" "$job_id" "cpu=${new_allotment}"
}

running_ids() {
    jq -r 'keys[]?' <<< "$RUNNING_JSON"
}

running_count() {
    jq -r 'length' <<< "$RUNNING_JSON"
}

append_history() {
    local history_json="$1"
    local value="$2"
    local max_len="$3"
    jq -nc --argjson history "$history_json" --argjson val "$value" --argjson max "$max_len" \
        '$history + [$val] | if length > $max then .[(length-$max):] else . end'
}

history_count() {
    local history_json="$1"
    jq -rn --argjson history "$history_json" '[$history[] | select(. == 1)] | length'
}

append_sample() {
    local samples_json="$1"
    local value="$2"
    jq -nc --argjson samples "$samples_json" --argjson val "$value" --argjson max "$SAMPLE_COUNT" \
        '$samples + [$val] | if length > $max then .[(length-$max):] else . end'
}

sample_average() {
    local samples_json="$1"
    jq -rn --argjson samples "$samples_json" 'if ($samples | length) > 0 then ($samples | add / length) else 0 end'
}

proc_cpu_host_pct() {
    local pid="$1"
    local total_cpu=0

    # IMPORTANT: Sum CPU across the ENTIRE process tree, not just the direct PID.
    # The .pid file contains the bash subshell PID, which uses ~0% CPU.
    # The actual work happens in grandchildren (e.g., bash -> uv -> python).
    # Without tree traversal, capacity checks see 0% and start too many jobs.
    local all_pids="$pid"
    local queue="$pid"

    while [ -n "$queue" ]; do
        local next_queue=""
        for p in $queue; do
            local children
            children=$(pgrep -P "$p" 2>/dev/null || true)
            if [ -n "$children" ]; then
                all_pids="$all_pids $children"
                next_queue="$next_queue $children"
            fi
        done
        queue="$next_queue"
    done

    # Sum CPU for all PIDs in the tree
    for p in $all_pids; do
        local cpu
        cpu=$(ps -p "$p" -o %cpu= 2>/dev/null | head -1 | tr -d ' ')
        if [ -n "$cpu" ]; then
            total_cpu=$(awk -v t="$total_cpu" -v c="$cpu" 'BEGIN { printf "%.1f", t + c }')
        fi
    done

    awk -v p="$total_cpu" -v c="$CPU_COUNT" 'BEGIN { if (c < 1) c = 1; printf "%.0f", (p / c) }'
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
                if running_contains "$job_id"; then
                    refresh_running_allotment "$job_id"
                fi
                # Cancel any pending stop - new work arrived
                if [ "$STOP_REQUESTED" = true ]; then
                    STOP_REQUESTED=false
                    stop_requested=false
                    echo "Stop cancelled: new work arrived"
                fi
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
                STOP_REQUESTED=true
                log_op "cmd.stop"
                echo "Command: stop requested"
                ;;
            restart)
                # Save state and re-exec to pick up new script version.
                # Running jobs are tracked in state file and will be recovered.
                log_op "cmd.restart"
                echo "Command: restart requested - re-execing to pick up new version"
                STATE_CURSOR="$ts"
                STATE_CURSOR_LINE="$line_num"
                save_state
                exec bash "$0" "$QUEUE_NAME"
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

# Check if a process is stopped (state T)
# Returns 0 (true) if the process exists and is in stopped state
process_stopped() {
    local pid="$1"
    [ -z "$pid" ] && return 1
    local state
    state=$(ps -o stat= -p "$pid" 2>/dev/null | tr -d ' ')
    case "$state" in
        *T*) return 0 ;;
        *) return 1 ;;
    esac
}

# Check if a job is currently running
job_running() {
    local job_id="$1"
    local pid_file="$LOG_DIR/${job_id}.pid"
    local pgid_file="$LOG_DIR/${job_id}.pgid"

    # Check wrapper process
    if [ -f "$pid_file" ]; then
        local pid
        pid=$(cat "$pid_file" 2>/dev/null | tail -1)
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            return 0
        fi
    fi

    # Check actual command process group (may survive if wrapper died)
    if [ -f "$pgid_file" ]; then
        local pgid
        pgid=$(cat "$pgid_file" 2>/dev/null)
        if [ -n "$pgid" ] && kill -0 "$pgid" 2>/dev/null; then
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

# Start a single job in the background
start_job() {
    local job_id="$1"
    local job_data
    job_data=$(get_job_data "$job_id")

    # Extract job fields using jq
    local working_dir command description deps_spec
    working_dir=$(echo "$job_data" | jq -r '.dir // ""')
    command=$(echo "$job_data" | jq -r '.cmd // ""')
    description=$(echo "$job_data" | jq -r '.desc // ""')
    deps_spec=$(echo "$job_data" | jq -r '.deps // ""')

    if [ -z "$command" ]; then
        echo "Job $job_id: no command found, skipping"
        return 1
    fi

    # IMPORTANT: Check completed/running BEFORE dependencies.
    # If a job already finished, skip it immediately. Otherwise a completed job
    # whose dependency was never created (or was cleaned up) will loop forever
    # waiting for a dependency that will never be satisfied.
    if job_completed "$job_id"; then
        echo "Job $job_id: already completed, skipping"
        rm -f "$QUEUE_DIR/job-${job_id}.json"
        return 0
    fi

    if job_running "$job_id"; then
        echo "Job $job_id: already running, skipping"
        return 0
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

    local start_time
    start_time=$(date +%s)

    # File paths
    local log_file="$LOG_DIR/${job_id}.log"
    local status_file="$LOG_DIR/${job_id}.status"
    local meta_file="$LOG_DIR/${job_id}.meta"
    local pid_file="$LOG_DIR/${job_id}.pid"
    local pgid_file="$LOG_DIR/${job_id}.pgid"

    # Archive any existing files from previous runs
    for ext in log status meta pid pgid samples; do
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

    (
        # Ignore SIGHUP so job survives if queue runner is killed/restarted
        trap '' HUP
        set +e
        # cd if working directory specified
        if [ -n "$working_dir" ]; then
            cd "$eval_working_dir" 2>/dev/null || {
                echo "ERROR: Could not cd to $working_dir"
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

        # Use setsid to run command in new session, immune to parent's SIGHUP
        # Run in background to capture the session leader PID for cleanup
        setsid bash -c "$command" &
        local setsid_pid=$!
        echo "$setsid_pid" > "$pgid_file"
        wait $setsid_pid
        exit_code=$?

        local end_time duration
        end_time=$(date +%s)
        duration=$((end_time - start_time))

        echo "$exit_code" > "$status_file"
        echo "=== END exit=$exit_code $(date) ==="

        if [ "$exit_code" -eq 0 ]; then
            log_op "job.completed" "$job_id" "exit=0 duration=${duration}s"
            echo "Job $job_id completed successfully"
        else
            log_op "job.failed" "$job_id" "exit=$exit_code duration=${duration}s"
            echo "Job $job_id failed with exit code $exit_code"
        fi

        if [ -x "$NOTIFY_SCRIPT" ]; then
            "$NOTIFY_SCRIPT" "rj-$job_id" "$exit_code" "$(hostname)" "$meta_file" 2>/dev/null || true
        fi

        rm -f "$pid_file" "$pgid_file" "$QUEUE_DIR/job-${job_id}.json"
        exit "$exit_code"
    ) >> "$log_file" 2>&1 &

    local cmd_pid=$!
    echo "$cmd_pid" > "$pid_file"

    local local_allotment
    local_allotment=$(job_allotment_from_data "$job_id")
    RUNNING_JSON=$(jq -nc \
        --argjson running "$RUNNING_JSON" \
        --arg id "$job_id" \
        --argjson started_at "$start_time" \
        --argjson warmup_until "$((start_time + WARMUP_DURATION))" \
        --argjson local_allotment "$local_allotment" \
        '$running + {($id): {started_at: $started_at, warmup_until: $warmup_until, local_allotment: $local_allotment, samples: [], over_hist: [], under_hist: []}}')

    CURRENT_JOB_ID="$job_id"
    echo "$CURRENT_JOB_ID" > "$CURRENT_FILE"
    save_state

    return 0
}

warmup_active() {
    local now
    now=$(date +%s)
    local ids
    ids=$(running_ids)
    for job_id in $ids; do
        local warmup_until
        warmup_until=$(jq -r --arg id "$job_id" '.[$id].warmup_until // 0' <<< "$RUNNING_JSON")
        if [ "$warmup_until" -gt "$now" ]; then
            return 0
        fi
    done
    return 1
}

total_local_allotment() {
    jq -r '[.[]?.local_allotment // 0] | add // 0' <<< "$RUNNING_JSON"
}

refresh_running_jobs() {
    local changed=false
    local ids
    ids=$(running_ids)
    for job_id in $ids; do
        local status_file="$LOG_DIR/${job_id}.status"
        local pid_file="$LOG_DIR/${job_id}.pid"
        local pgid_file="$LOG_DIR/${job_id}.pgid"

        if [ -f "$status_file" ]; then
            # Record CPU history before removing job from running state
            local samples_json avg_cpu job_data cmd sig
            samples_json=$(jq -c --arg id "$job_id" '.[$id].samples // []' <<< "$RUNNING_JSON")
            avg_cpu=$(sample_average "$samples_json")
            if [ -n "$avg_cpu" ] && [ "$avg_cpu" != "0" ]; then
                job_data=$(get_job_data "$job_id")
                cmd=$(echo "$job_data" | jq -r '.cmd // ""')
                sig=$(extract_command_signature "$cmd")
                if [ -n "$sig" ]; then
                    update_cpu_history "$sig" "$avg_cpu"
                fi
            fi
            remove_running_job "$job_id"
            changed=true
            continue
        fi

        # Check if the job's process is stopped (state T).
        # A stopped process is alive but not executing - this can happen if:
        # - The user sent SIGSTOP (ctrl-z)
        # - The system's OOM killer stopped it
        # - Some external process stopped it
        # We treat stopped jobs as failed since they won't make progress.
        local pgid=""
        local pid=""
        if [ -f "$pgid_file" ]; then
            pgid=$(cat "$pgid_file" 2>/dev/null)
        fi
        if [ -f "$pid_file" ]; then
            pid=$(cat "$pid_file" 2>/dev/null | tail -1)
        fi

        # Check PGID first (the actual command), then PID (the wrapper)
        local check_pid="${pgid:-$pid}"
        if [ -n "$check_pid" ] && process_stopped "$check_pid"; then
            echo "Job $job_id process $check_pid is stopped (state T) - marking as failed"
            log_op "job.stopped_detected" "$job_id" "pid=$check_pid state=T"
            # Kill the stopped process group to clean up
            if [ -n "$pgid" ]; then
                kill -KILL -"$pgid" 2>/dev/null || true
            fi
            if [ -n "$pid" ] && [ "$pid" != "$pgid" ]; then
                kill -KILL "$pid" 2>/dev/null || true
            fi
            echo "1" > "$status_file"
            log_op "job.failed" "$job_id" "exit=1 reason=stopped"
            rm -f "$pid_file" "$pgid_file"
            remove_running_job "$job_id"
            changed=true
            continue
        fi

        # Check if wrapper process is still running
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            continue
        fi

        # Wrapper process is gone - check if orphaned command processes remain
        # and kill them before marking job as failed
        if [ -f "$pgid_file" ] && [ -n "$pgid" ] && kill -0 "$pgid" 2>/dev/null; then
            # Process group still running - kill it
            echo "Killing orphaned process group $pgid for job $job_id"
            kill -TERM -"$pgid" 2>/dev/null || true
            sleep 1
            kill -KILL -"$pgid" 2>/dev/null || true
            log_op "job.orphan_killed" "$job_id" "pgid=$pgid"
        fi
        rm -f "$pgid_file"

        if [ ! -f "$status_file" ]; then
            echo "1" > "$status_file"
            log_op "job.failed" "$job_id" "exit=1 duration=0"
        fi
        remove_running_job "$job_id"
        changed=true
    done
    update_current_from_running
    if [ "$changed" = true ]; then
        save_state
    fi
}

sample_running_jobs() {
    local now
    now=$(date +%s)
    if [ "$LAST_SAMPLE_TIME" -ne 0 ] && [ $((now - LAST_SAMPLE_TIME)) -lt "$SAMPLE_INTERVAL" ]; then
        return
    fi
    LAST_SAMPLE_TIME="$now"

    local updated=false
    local ids
    ids=$(running_ids)
    for job_id in $ids; do
        local pid_file="$LOG_DIR/${job_id}.pid"
        local warmup_until
        warmup_until=$(jq -r --arg id "$job_id" '.[$id].warmup_until // 0' <<< "$RUNNING_JSON")
        if [ "$warmup_until" -gt "$now" ]; then
            continue
        fi
        local pid
        pid=$(cat "$pid_file" 2>/dev/null | tail -1)
        if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
            continue
        fi

        local host_pct
        host_pct=$(proc_cpu_host_pct "$pid")
        local samples_file="$LOG_DIR/${job_id}.samples"
        echo "$now $host_pct" >> "$samples_file"
        local samples_json
        samples_json=$(jq -c --arg id "$job_id" '.[$id].samples // []' <<< "$RUNNING_JSON")
        samples_json=$(append_sample "$samples_json" "$host_pct")
        RUNNING_JSON=$(jq -c --arg id "$job_id" --argjson samples "$samples_json" '.[$id].samples = $samples' <<< "$RUNNING_JSON")

        local avg
        avg=$(sample_average "$samples_json")
        local sample_count
        sample_count=$(jq -rn --argjson samples "$samples_json" '($samples | length)')
        if [ "$sample_count" -lt "$SAMPLE_COUNT" ]; then
            updated=true
            continue
        fi
        local local_allotment
        local_allotment=$(jq -r --arg id "$job_id" '.[$id].local_allotment // 0' <<< "$RUNNING_JSON")

        local over under
        over=$(awk -v avg="$avg" -v allot="$local_allotment" 'BEGIN {print (avg > allot) ? 1 : 0}')
        under=$(awk -v avg="$avg" -v allot="$local_allotment" 'BEGIN {print (avg < (allot - 10)) ? 1 : 0}')

        local over_hist
        local under_hist
        over_hist=$(jq -c --arg id "$job_id" '.[$id].over_hist // []' <<< "$RUNNING_JSON")
        under_hist=$(jq -c --arg id "$job_id" '.[$id].under_hist // []' <<< "$RUNNING_JSON")
        over_hist=$(append_history "$over_hist" "$over" "$HYSTERESIS_WINDOW")
        under_hist=$(append_history "$under_hist" "$under" "$HYSTERESIS_WINDOW")
        RUNNING_JSON=$(jq -c \
            --arg id "$job_id" \
            --argjson over_hist "$over_hist" \
            --argjson under_hist "$under_hist" \
            '.[$id].over_hist = $over_hist | .[$id].under_hist = $under_hist' <<< "$RUNNING_JSON")

        local over_count under_count
        over_count=$(history_count "$over_hist")
        under_count=$(history_count "$under_hist")

        if [ "$over_count" -ge "$HYSTERESIS_THRESHOLD" ]; then
            # Calculate headroom: how much can this job's allotment grow without
            # pushing total above HOST_UTILIZATION_TARGET?
            local total_allotment
            total_allotment=$(jq -r '[.[]?.local_allotment // 0] | add // 0' <<< "$RUNNING_JSON")
            local headroom
            headroom=$((HOST_UTILIZATION_TARGET - total_allotment + local_allotment))

            local new_allotment
            new_allotment=$(awk -v avg="$avg" -v step="$INCREASE_STEP" -v max="$MAX_ALLOTMENT" -v head="$headroom" \
                'BEGIN {v = avg + step; if (v > max) v = max; if (v > head) v = head; if (v < 0) v = 0; printf "%.0f", v}')
            if [ "$new_allotment" -lt "$MIN_ALLOTMENT" ]; then
                new_allotment="$MIN_ALLOTMENT"
            fi
            # Only update if there's actually room to grow
            if [ "$new_allotment" -gt "$local_allotment" ]; then
                RUNNING_JSON=$(jq -c \
                    --arg id "$job_id" \
                    --argjson allot "$new_allotment" \
                    '.[$id].local_allotment = $allot | .[$id].samples = [] | .[$id].over_hist = [] | .[$id].under_hist = []' <<< "$RUNNING_JSON")
                log_op "job.allotment_increase" "$job_id" "cpu=${new_allotment} observed=${avg} headroom=${headroom}"
            fi
            updated=true
            continue
        fi

        if [ "$under_count" -ge "$HYSTERESIS_THRESHOLD" ]; then
            local new_allotment
            new_allotment=$((local_allotment - DECAY_STEP))
            if [ "$new_allotment" -lt "$MIN_ALLOTMENT" ]; then
                new_allotment="$MIN_ALLOTMENT"
            fi
            RUNNING_JSON=$(jq -c \
                --arg id "$job_id" \
                --argjson allot "$new_allotment" \
                '.[$id].local_allotment = $allot | .[$id].samples = [] | .[$id].over_hist = [] | .[$id].under_hist = []' <<< "$RUNNING_JSON")
            log_op "job.allotment_decay" "$job_id" "cpu=${new_allotment} observed=${avg}"
            updated=true
        fi
    done

    if [ "$updated" = true ]; then
        save_state
    fi
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
    process_commands || true

    # Refresh running jobs and sampling
    refresh_running_jobs
    sample_running_jobs

    # Stop requested: exit once pending and running are empty
    if [ "$STOP_REQUESTED" = true ] && pending_empty && [ "$(running_count)" -eq 0 ]; then
        log_op "queue.stop" "" "stop command received"
        echo "Stop command received, exiting..."
        break
    fi

    # Check if we have pending jobs
    if pending_empty; then
        sleep 5
        continue
    fi

    if warmup_active; then
        sleep 2
        continue
    fi
    if [ "$STOP_REQUESTED" = true ]; then
        sleep 5
        continue
    fi

    # Get next job (pop_pending sets POPPED_JOB global, can't use $() - subshell loses state)
    pop_pending

    if [ -z "$POPPED_JOB" ]; then
        sleep 5
        continue
    fi

    # Check capacity before starting
    # Always allow at least one job when nothing is running, even if its predicted
    # allotment exceeds the target (otherwise jobs with high allotment never start)
    current_allotment=$(total_local_allotment)
    next_allotment=$(job_allotment_from_data "$POPPED_JOB")
    running_jobs=$(running_count)
    if [ "$running_jobs" -gt 0 ] && [ $((current_allotment + next_allotment)) -gt "$HOST_UTILIZATION_TARGET" ]; then
        add_pending "$POPPED_JOB"
        save_state
        sleep 5
        continue
    fi

    # Start the job
    run_result=0
    start_job "$POPPED_JOB" || run_result=$?

    case "$run_result" in
        2)
            # Re-queue (dependency waiting)
            add_pending "$POPPED_JOB"
            save_state
            sleep 10
            ;;
        *)
            save_state
            ;;
    esac
done

log_op "queue.stop" "" "normal exit"
echo "Queue runner exiting"
