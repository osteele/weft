#!/bin/bash
#
# View log file for a remote job
# Also updates the job database if status has changed
#
# Usage:
#   view-log.sh <host> <session-name> [--follow|-f] [--lines|-n <N>]
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/db.sh"

# Check and update job status in database
update_job_status() {
    local host="$1"
    local session_name="$2"

    # Only update if we have a running job in the database
    local db_status
    db_status=$(db_get_status "$host" "$session_name")

    if [ "$db_status" != "running" ]; then
        return
    fi

    # Check if session still exists
    local session_exists
    session_exists=$(ssh "$host" "tmux has-session -t '$session_name' 2>&1 && echo EXISTS || echo NOTEXISTS" 2>/dev/null || echo "NOTEXISTS")

    if echo "$session_exists" | grep -qw "NOTEXISTS"; then
        # Session gone - check for status file
        local status_file="/tmp/tmux-${session_name}.status"
        local exit_code
        exit_code=$(ssh "$host" "cat '$status_file' 2>/dev/null" || echo "")

        if [ -n "$exit_code" ]; then
            db_record_completion "$host" "$session_name" "$exit_code" "$(date +%s)" "completed"
        else
            db_mark_dead "$host" "$session_name"
        fi
        return
    fi

    # Session exists - check if job is still running
    local pane_pid
    pane_pid=$(ssh "$host" "tmux list-panes -t '$session_name' -F '#{pane_pid}' 2>/dev/null | head -1" || echo "")

    if [ -n "$pane_pid" ]; then
        local child_count
        child_count=$(ssh "$host" "pgrep -P '$pane_pid' 2>/dev/null | wc -l" | tr -d ' ')

        if [ "$child_count" -eq 0 ]; then
            # Job finished
            local status_file="/tmp/tmux-${session_name}.status"
            local exit_code
            exit_code=$(ssh "$host" "cat '$status_file' 2>/dev/null" || echo "")

            if [ -n "$exit_code" ]; then
                db_record_completion "$host" "$session_name" "$exit_code" "$(date +%s)" "completed"
            fi
        fi
    fi
}

show_usage() {
    echo "Usage: $0 <host> <session-name> [options]"
    echo ""
    echo "Options:"
    echo "  -f, --follow     Follow log output (like tail -f)"
    echo "  -n, --lines N    Show last N lines (default: 50)"
    echo ""
    echo "Example:"
    echo "  $0 cool30 train-gpt2"
    echo "  $0 cool30 train-gpt2 -f"
    echo "  $0 cool30 train-gpt2 -n 100"
}

if [ $# -lt 2 ]; then
    show_usage
    exit 1
fi

HOST="$1"
SESSION_NAME="$2"
shift 2

FOLLOW=false
LINES=50

while [ $# -gt 0 ]; do
    case "$1" in
        -f|--follow)
            FOLLOW=true
            shift
            ;;
        -n|--lines)
            LINES="$2"
            shift 2
            ;;
        *)
            echo "Unknown option: $1"
            show_usage
            exit 1
            ;;
    esac
done

LOG_FILE="/tmp/tmux-${SESSION_NAME}.log"

# Update job status in database (if job has finished)
update_job_status "$HOST" "$SESSION_NAME"

# Check if log file exists
if ! ssh "$HOST" "test -f '$LOG_FILE'"; then
    echo "ERROR: Log file not found: $LOG_FILE"
    echo "Session '$SESSION_NAME' may not exist or hasn't produced output yet."
    exit 1
fi

if [ "$FOLLOW" = true ]; then
    echo "Following log for '$SESSION_NAME' on $HOST (Ctrl+C to stop)..."
    echo "---"
    ssh "$HOST" "tail -f '$LOG_FILE'"
else
    echo "Last $LINES lines of '$SESSION_NAME' on $HOST:"
    echo "---"
    ssh "$HOST" "tail -n $LINES '$LOG_FILE'"
fi
