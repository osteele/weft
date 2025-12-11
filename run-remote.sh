#!/bin/bash
#
# Persistent job runner using tmux on remote host
#
# Usage:
#   run-remote.sh [options] <host> <command...>
#
# Options:
#   -n, --name NAME         Session name (default: auto-generated from command)
#   -C, --directory DIR     Working directory (default: same path as local cwd)
#   -d, --description TEXT  Description of the job (for logging and queries)
#   --queue                 Queue job for later instead of running now
#   --queue-on-fail         Queue job if connection fails
#
# Examples:
#   run-remote.sh cool30 'python train.py'
#   run-remote.sh -d "Training GPT-2" cool30 'with-gpu python train.py'
#   run-remote.sh -n train-gpt2 -C /mnt/code/LM2 cool30 'python train.py'
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Source database functions
source "$SCRIPT_DIR/db.sh"

# Generate a session name from a command
# e.g., "python train.py" -> "python-train"
# e.g., "with-gpu python script.py" -> "python-script"
generate_session_name() {
    local cmd="$1"
    # Skip common prefixes like 'with-gpu'
    local cleaned
    cleaned=$(echo "$cmd" | sed -E 's/^(with-gpu|env [^ ]+) //')
    # Extract first two words, remove file extensions, join with dash
    echo "$cleaned" | awk '{
        # Get program name (first word)
        prog = $1
        sub(/.*\//, "", prog)  # Remove path
        # Get first arg (second word)
        arg = $2
        sub(/.*\//, "", arg)   # Remove path
        sub(/\.[^.]+$/, "", arg)  # Remove extension
        # Remove common flags
        if (arg ~ /^-/) arg = ""
        if (arg != "") {
            print prog "-" arg
        } else {
            print prog
        }
    }' | tr -cd 'a-zA-Z0-9-' | head -c 30
}

# Get default working directory (convert local path to remote-friendly path)
# /Users/osteele/code/LM2 -> ~/code/LM2
default_working_dir() {
    local cwd
    cwd=$(pwd)
    # Replace home directory with ~
    echo "$cwd" | sed "s|^$HOME|~|"
}

# Retry configuration
MAX_RETRIES=5
RETRY_DELAY=30

# Retry wrapper for ssh commands that handles connection timeouts and unreachable hosts
# Usage: ssh_with_retry <ssh_args...>
ssh_with_retry() {
    local attempt=1
    local output
    local exit_code

    while [ $attempt -le $MAX_RETRIES ]; do
        set +e
        output=$(ssh "$@" 2>&1)
        exit_code=$?
        set -e

        # Check for connection timeout or host unreachable errors
        if [ $exit_code -ne 0 ]; then
            if echo "$output" | grep -qiE "(connection timed out|no route to host|host is unreachable|connection refused|network is unreachable)"; then
                if [ $attempt -lt $MAX_RETRIES ]; then
                    echo "Connection failed (attempt $attempt/$MAX_RETRIES): $output" >&2
                    echo "Retrying in ${RETRY_DELAY}s..." >&2
                    sleep $RETRY_DELAY
                    attempt=$((attempt + 1))
                    continue
                else
                    echo "Connection failed after $MAX_RETRIES attempts: $output" >&2
                    return $exit_code
                fi
            else
                # Non-connection error, don't retry
                echo "$output"
                return $exit_code
            fi
        fi

        # Success
        echo "$output"
        return 0
    done
}

# Retry wrapper for scp commands
# Usage: scp_with_retry <scp_args...>
scp_with_retry() {
    local attempt=1
    local output
    local exit_code

    while [ $attempt -le $MAX_RETRIES ]; do
        set +e
        output=$(scp "$@" 2>&1)
        exit_code=$?
        set -e

        if [ $exit_code -ne 0 ]; then
            if echo "$output" | grep -qiE "(connection timed out|no route to host|host is unreachable|connection refused|network is unreachable)"; then
                if [ $attempt -lt $MAX_RETRIES ]; then
                    echo "SCP failed (attempt $attempt/$MAX_RETRIES): $output" >&2
                    echo "Retrying in ${RETRY_DELAY}s..." >&2
                    sleep $RETRY_DELAY
                    attempt=$((attempt + 1))
                    continue
                else
                    echo "SCP failed after $MAX_RETRIES attempts: $output" >&2
                    return $exit_code
                fi
            else
                echo "$output"
                return $exit_code
            fi
        fi

        echo "$output"
        return 0
    done
}

show_usage() {
    echo "Usage: $0 [options] <host> <command...>"
    echo ""
    echo "Options:"
    echo "  -n, --name NAME         Session name (default: auto-generated from command)"
    echo "  -C, --directory DIR     Working directory (default: same path as local cwd)"
    echo "  -d, --description TEXT  Description of the job (for logging and queries)"
    echo "  --queue                 Queue job for later instead of running now"
    echo "  --queue-on-fail         Queue job if connection fails"
    echo ""
    echo "Examples:"
    echo "  $0 cool30 'python train.py'"
    echo "  $0 -d 'Training run' cool30 'python train.py --lr 0.001'"
    echo "  $0 -n train-gpt2 -C /mnt/code/LM2 cool30 'with-gpu python train.py'"
    echo "  $0 --queue cool30 'python train.py'  # Queue for later"
}

# Parse optional flags
DESCRIPTION=""
SESSION_NAME=""
WORKING_DIR=""
QUEUE_ONLY=false
QUEUE_ON_FAIL=false
while [ $# -gt 0 ]; do
    case "$1" in
        -n|--name)
            SESSION_NAME="$2"
            shift 2
            ;;
        -C|--directory)
            WORKING_DIR="$2"
            shift 2
            ;;
        -d|--description)
            DESCRIPTION="$2"
            shift 2
            ;;
        --queue)
            QUEUE_ONLY=true
            shift
            ;;
        --queue-on-fail)
            QUEUE_ON_FAIL=true
            shift
            ;;
        -h|--help)
            show_usage
            exit 0
            ;;
        -*)
            echo "Unknown option: $1"
            show_usage
            exit 1
            ;;
        *)
            break
            ;;
    esac
done

if [ $# -lt 2 ]; then
    show_usage
    exit 1
fi

HOST="$1"
shift
COMMAND="$*"

# Set defaults for session name and working directory
if [ -z "$SESSION_NAME" ]; then
    SESSION_NAME=$(generate_session_name "$COMMAND")
fi
if [ -z "$WORKING_DIR" ]; then
    WORKING_DIR=$(default_working_dir)
fi

# Queue-only mode: just save to pending and exit
if [ "$QUEUE_ONLY" = true ]; then
    JOB_ID=$(db_record_pending "$HOST" "$SESSION_NAME" "$WORKING_DIR" "$COMMAND" "$DESCRIPTION")
    echo "Job queued with ID: $JOB_ID"
    echo ""
    echo "  Host: $HOST"
    echo "  Session: $SESSION_NAME"
    echo "  Working dir: $WORKING_DIR"
    echo "  Command: $COMMAND"
    if [ -n "$DESCRIPTION" ]; then
        echo "  Description: $DESCRIPTION"
    fi
    echo ""
    echo "To start this job:"
    echo "  $SCRIPT_DIR/retry-job.sh $JOB_ID"
    exit 0
fi

# Check if session already exists on remote
# Note: tmux has-session returns non-zero if session doesn't exist, so we check output
set +e
session_check=$(ssh_with_retry "$HOST" "tmux has-session -t '$SESSION_NAME' 2>&1 && echo EXISTS || echo NOTEXISTS")
ssh_exit_code=$?
set -e

# Handle connection failures
if [ $ssh_exit_code -ne 0 ]; then
    if [ "$QUEUE_ON_FAIL" = true ]; then
        echo "Connection failed. Queuing job for later..."
        JOB_ID=$(db_record_pending "$HOST" "$SESSION_NAME" "$WORKING_DIR" "$COMMAND" "$DESCRIPTION")
        echo "Job queued with ID: $JOB_ID"
        echo ""
        echo "To retry when connection is available:"
        echo "  $SCRIPT_DIR/retry-job.sh $JOB_ID"
        exit 0
    else
        echo "ERROR: Could not connect to $HOST"
        echo ""
        echo "To queue this job for later, use --queue-on-fail:"
        echo "  $0 --queue-on-fail -d '$DESCRIPTION' $HOST $SESSION_NAME $WORKING_DIR $COMMAND"
        exit 1
    fi
fi
if echo "$session_check" | grep -qw "EXISTS"; then
    echo "ERROR: Session '$SESSION_NAME' already exists on $HOST"
    echo "Attach with: ssh $HOST -t 'tmux attach -t $SESSION_NAME'"
    echo "Or kill with: ssh $HOST 'tmux kill-session -t $SESSION_NAME'"
    exit 1
fi

# Create file paths
LOG_FILE="/tmp/tmux-${SESSION_NAME}.log"
STATUS_FILE="/tmp/tmux-${SESSION_NAME}.status"
METADATA_FILE="/tmp/tmux-${SESSION_NAME}.meta"

# Archive any existing log file with timestamp
ssh_with_retry "$HOST" "if [ -f '$LOG_FILE' ]; then mv '$LOG_FILE' '${LOG_FILE%.log}.\$(date +%Y%m%d-%H%M%S).log'; fi"

# Remove old status file
ssh_with_retry "$HOST" "rm -f '$STATUS_FILE'"

# Create new detached session on remote and exit immediately
# This ensures the script doesn't block (important for coding agents)
echo "Starting persistent session '$SESSION_NAME' on $HOST"
echo "Working directory: $WORKING_DIR"
echo "Command: $COMMAND"
if [ -n "$DESCRIPTION" ]; then
    echo "Description: $DESCRIPTION"
fi
echo "Log file: $LOG_FILE"
echo ""

# Save metadata for restart capability
START_TIME=$(date +%s)
ssh_with_retry "$HOST" "cat > '$METADATA_FILE'" << EOF
working_dir=$WORKING_DIR
command=$COMMAND
start_time=$START_TIME
host=$HOST
description=$DESCRIPTION
EOF

# Copy notify script to remote host and set up Slack notification
REMOTE_NOTIFY_SCRIPT="/tmp/remote-jobs-notify-slack.sh"
NOTIFY_CMD=""

# Check for Slack webhook configuration (env var or config file)
SLACK_WEBHOOK="${REMOTE_JOBS_SLACK_WEBHOOK:-}"
if [ -z "$SLACK_WEBHOOK" ] && [ -f ~/.config/remote-jobs/config ]; then
    SLACK_WEBHOOK=$(grep '^SLACK_WEBHOOK=' ~/.config/remote-jobs/config 2>/dev/null | cut -d= -f2- || true)
fi

if [ -n "$SLACK_WEBHOOK" ]; then
    # Copy notify script to remote
    scp -q "$SCRIPT_DIR/notify-slack.sh" "$HOST:$REMOTE_NOTIFY_SCRIPT"
    ssh "$HOST" "chmod +x '$REMOTE_NOTIFY_SCRIPT'"
    # Export webhook URL to remote environment and add notify command
    NOTIFY_CMD="; REMOTE_JOBS_SLACK_WEBHOOK='$SLACK_WEBHOOK' '$REMOTE_NOTIFY_SCRIPT' '$SESSION_NAME' \\\$EXIT_CODE '$HOST'"
    echo "Slack notifications: enabled"
fi

# Wrap command to redirect stdout/stderr to log file and capture exit code
# Note: Use \\\$ to ensure $ is preserved through local shell -> ssh -> bash -c
WRAPPED_COMMAND="cd '$WORKING_DIR' && $COMMAND 2>&1 | tee '$LOG_FILE'; EXIT_CODE=\\\$?; echo \\\$EXIT_CODE > '$STATUS_FILE'$NOTIFY_CMD"

ssh "$HOST" "tmux new-session -d -s '$SESSION_NAME' bash -c \"$WRAPPED_COMMAND\"" && echo "✓ Session started successfully"

# Record job in local database
JOB_ID=$(db_record_start "$HOST" "$SESSION_NAME" "$WORKING_DIR" "$COMMAND" "$START_TIME" "$DESCRIPTION")
echo "Job ID: $JOB_ID"

echo ""
echo "Monitor progress:"
echo "  ssh $HOST -t 'tmux attach -t $SESSION_NAME'  # Attach (Ctrl+B D to detach)"
echo "  ssh $HOST 'tmux capture-pane -t $SESSION_NAME -p | tail -20'  # View last 20 lines"
echo "  ssh $HOST 'tail -f $LOG_FILE'  # Follow log file"
echo ""
echo "Check status:"
echo "  ~/code/utilities/remote-jobs/check-jobs.sh $HOST"
