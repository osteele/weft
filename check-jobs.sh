#!/bin/bash
#
# Check status of all persistent tmux sessions on remote host
# Updates the local database when job status changes are detected
#
# Usage:
#   check-jobs.sh <host>
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/db.sh"

if [ $# -lt 1 ]; then
    echo "Usage: $0 <host>"
    echo ""
    echo "Example:"
    echo "  $0 cool30"
    exit 1
fi

HOST="$1"

echo "Active tmux sessions on $HOST:"
echo ""

# List all sessions
ssh "$HOST" 'tmux list-sessions 2>/dev/null' || echo "  (none)"
echo ""

# For each session, show status and last few lines of output
for session in $(ssh "$HOST" 'tmux list-sessions -F "#{session_name}" 2>/dev/null'); do
    # Check if the pane still has a running process (pane_pid will have child processes)
    pane_pid=$(ssh "$HOST" "tmux list-panes -t '$session' -F '#{pane_pid}' 2>/dev/null | head -1")
    if [ -n "$pane_pid" ]; then
        # Check if the shell has any child processes (the actual job)
        child_count=$(ssh "$HOST" "pgrep -P '$pane_pid' 2>/dev/null | wc -l" | tr -d ' ')
        if [ "$child_count" -gt 0 ]; then
            status="RUNNING"
            exit_info=""
        else
            status="FINISHED"
            # Check for exit code in status file
            status_file="/tmp/tmux-${session}.status"
            exit_code=$(ssh "$HOST" "cat '$status_file' 2>/dev/null" || echo "")
            if [ -n "$exit_code" ]; then
                if [ "$exit_code" -eq 0 ]; then
                    exit_info=" (exit: 0 ✓)"
                else
                    exit_info=" (exit: $exit_code ✗)"
                fi
                # Update database if job was marked as running
                db_status=$(db_get_status "$HOST" "$session")
                if [ "$db_status" = "running" ]; then
                    db_record_completion "$HOST" "$session" "$exit_code" "$(date +%s)" "completed"
                fi
            else
                exit_info=""
            fi
        fi
    else
        status="UNKNOWN"
        exit_info=""
    fi

    echo "=== Session: $session [$status]$exit_info ==="
    ssh "$HOST" "tmux capture-pane -t '$session' -p | tail -10"
    echo ""
done

# Check for jobs marked as running in DB but no longer have sessions
echo "Checking for dead jobs..."
for job_info in $(sqlite3 -separator ':' "$DB_FILE" "SELECT session_name FROM jobs WHERE host = '$HOST' AND status = 'running';" 2>/dev/null); do
    session_exists=$(ssh "$HOST" "tmux has-session -t '$job_info' 2>&1 && echo EXISTS || echo NOTEXISTS")
    if echo "$session_exists" | grep -qw "NOTEXISTS"; then
        # Check for status file first
        status_file="/tmp/tmux-${job_info}.status"
        exit_code=$(ssh "$HOST" "cat '$status_file' 2>/dev/null" || echo "")
        if [ -n "$exit_code" ]; then
            db_record_completion "$HOST" "$job_info" "$exit_code" "$(date +%s)" "completed"
            echo "  Updated: $job_info completed (exit: $exit_code)"
        else
            db_mark_dead "$HOST" "$job_info"
            echo "  Updated: $job_info marked as dead"
        fi
    fi
done
