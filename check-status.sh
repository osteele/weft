#!/bin/bash
#
# Check status of a specific remote job
# Uses database as quick path for terminated jobs
#
# Usage:
#   check-status.sh <host> <session-name>
#
# Exit codes:
#   0 - Job completed successfully (exit code 0)
#   1 - Job failed (non-zero exit code) or error
#   2 - Job is still running
#   3 - Job not found
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/db.sh"

show_usage() {
    echo "Usage: $0 <host> <session-name>"
    echo ""
    echo "Check the status of a specific remote job."
    echo "Uses local database for quick lookup of terminated jobs."
    echo ""
    echo "Exit codes:"
    echo "  0 - Job completed successfully"
    echo "  1 - Job failed or error"
    echo "  2 - Job is still running"
    echo "  3 - Job not found"
    echo ""
    echo "Example:"
    echo "  $0 cool30 train-gpt2"
}

if [ $# -lt 2 ]; then
    show_usage
    exit 1
fi

HOST="$1"
SESSION_NAME="$2"

# Quick path: check database first
db_status=$(db_get_status "$HOST" "$SESSION_NAME")

if [ -z "$db_status" ]; then
    echo "Job not found in database: $SESSION_NAME on $HOST"
    exit 3
fi

# If already terminated in DB, return cached result
if [ "$db_status" = "completed" ] || [ "$db_status" = "dead" ]; then
    db_show_job "$HOST" "$SESSION_NAME"
    echo ""

    # Get exit code for return value
    exit_code=$(sqlite3 "$DB_FILE" "SELECT COALESCE(exit_code, 1) FROM jobs
        WHERE host = '$(echo "$HOST" | sed "s/'/''/g")'
          AND session_name = '$(echo "$SESSION_NAME" | sed "s/'/''/g")'
        ORDER BY start_time DESC LIMIT 1;")

    if [ "$db_status" = "dead" ]; then
        echo "Status: DEAD (job terminated unexpectedly)"
        exit 1
    elif [ "$exit_code" -eq 0 ]; then
        echo "Status: COMPLETED SUCCESSFULLY"
        exit 0
    else
        echo "Status: COMPLETED WITH FAILURE (exit code: $exit_code)"
        exit 1
    fi
fi

# Job is marked as running in DB - check actual status on remote
echo "Checking remote status..."

# Check if session exists
session_exists=$(ssh "$HOST" "tmux has-session -t '$SESSION_NAME' 2>&1 && echo EXISTS || echo NOTEXISTS")

if echo "$session_exists" | grep -qw "NOTEXISTS"; then
    # Session doesn't exist - job died without completing normally
    echo "Session no longer exists on remote host"

    # Check for status file (in case it completed but session was cleaned up)
    STATUS_FILE="/tmp/tmux-${SESSION_NAME}.status"
    exit_code=$(ssh "$HOST" "cat '$STATUS_FILE' 2>/dev/null" || echo "")

    if [ -n "$exit_code" ]; then
        # Job completed but session was cleaned up
        end_time=$(date +%s)
        db_record_completion "$HOST" "$SESSION_NAME" "$exit_code" "$end_time" "completed"

        db_show_job "$HOST" "$SESSION_NAME"
        echo ""
        if [ "$exit_code" -eq 0 ]; then
            echo "Status: COMPLETED SUCCESSFULLY"
            exit 0
        else
            echo "Status: COMPLETED WITH FAILURE (exit code: $exit_code)"
            exit 1
        fi
    else
        # No status file - job died unexpectedly
        db_mark_dead "$HOST" "$SESSION_NAME"

        db_show_job "$HOST" "$SESSION_NAME"
        echo ""
        echo "Status: DEAD (session terminated without exit code)"
        exit 1
    fi
fi

# Session exists - check if job is still running
pane_pid=$(ssh "$HOST" "tmux list-panes -t '$SESSION_NAME' -F '#{pane_pid}' 2>/dev/null | head -1")

if [ -n "$pane_pid" ]; then
    child_count=$(ssh "$HOST" "pgrep -P '$pane_pid' 2>/dev/null | wc -l" | tr -d ' ')

    if [ "$child_count" -gt 0 ]; then
        # Job is still running
        db_show_job "$HOST" "$SESSION_NAME"
        echo ""
        echo "Status: RUNNING"
        echo ""
        echo "Last 5 lines of output:"
        echo "---"
        ssh "$HOST" "tail -5 '/tmp/tmux-${SESSION_NAME}.log' 2>/dev/null" || echo "(no output yet)"
        exit 2
    else
        # Job finished - get exit code
        STATUS_FILE="/tmp/tmux-${SESSION_NAME}.status"
        exit_code=$(ssh "$HOST" "cat '$STATUS_FILE' 2>/dev/null" || echo "")

        if [ -n "$exit_code" ]; then
            end_time=$(date +%s)
            db_record_completion "$HOST" "$SESSION_NAME" "$exit_code" "$end_time" "completed"

            db_show_job "$HOST" "$SESSION_NAME"
            echo ""
            if [ "$exit_code" -eq 0 ]; then
                echo "Status: COMPLETED SUCCESSFULLY"
                exit 0
            else
                echo "Status: COMPLETED WITH FAILURE (exit code: $exit_code)"
                exit 1
            fi
        else
            # No exit code yet - still finishing up
            db_show_job "$HOST" "$SESSION_NAME"
            echo ""
            echo "Status: FINISHING (waiting for exit code)"
            exit 2
        fi
    fi
else
    echo "Cannot determine job status (no pane PID)"
    exit 1
fi
