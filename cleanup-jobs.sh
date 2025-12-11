#!/bin/bash
#
# Clean up finished tmux sessions and old log files on remote host
#
# Usage:
#   cleanup-jobs.sh <host> [--sessions] [--logs] [--older-than <days>] [--dry-run]
#

set -euo pipefail

show_usage() {
    echo "Usage: $0 <host> [options]"
    echo ""
    echo "Options:"
    echo "  --sessions       Kill all finished tmux sessions"
    echo "  --logs           Remove archived log files (*.YYYYMMDD-HHMMSS.log)"
    echo "  --older-than N   Only clean items older than N days (default: 7)"
    echo "  --dry-run        Show what would be cleaned without doing it"
    echo ""
    echo "If no --sessions or --logs specified, both are cleaned."
    echo ""
    echo "Example:"
    echo "  $0 cool30                     # Clean both sessions and logs"
    echo "  $0 cool30 --sessions          # Only clean finished sessions"
    echo "  $0 cool30 --logs --older-than 3  # Clean logs older than 3 days"
    echo "  $0 cool30 --dry-run           # Preview what would be cleaned"
}

if [ $# -lt 1 ]; then
    show_usage
    exit 1
fi

HOST="$1"
shift

CLEAN_SESSIONS=false
CLEAN_LOGS=false
OLDER_THAN=7
DRY_RUN=false

while [ $# -gt 0 ]; do
    case "$1" in
        --sessions)
            CLEAN_SESSIONS=true
            shift
            ;;
        --logs)
            CLEAN_LOGS=true
            shift
            ;;
        --older-than)
            OLDER_THAN="$2"
            shift 2
            ;;
        --dry-run)
            DRY_RUN=true
            shift
            ;;
        *)
            echo "Unknown option: $1"
            show_usage
            exit 1
            ;;
    esac
done

# If neither specified, clean both
if [ "$CLEAN_SESSIONS" = false ] && [ "$CLEAN_LOGS" = false ]; then
    CLEAN_SESSIONS=true
    CLEAN_LOGS=true
fi

if [ "$DRY_RUN" = true ]; then
    echo "[DRY RUN] Would perform the following actions:"
    echo ""
fi

# Clean finished sessions
if [ "$CLEAN_SESSIONS" = true ]; then
    echo "Checking for finished sessions on $HOST..."

    finished_sessions=()
    for session in $(ssh "$HOST" 'tmux list-sessions -F "#{session_name}" 2>/dev/null' || true); do
        pane_pid=$(ssh "$HOST" "tmux list-panes -t '$session' -F '#{pane_pid}' 2>/dev/null | head -1" || true)
        if [ -n "$pane_pid" ]; then
            child_count=$(ssh "$HOST" "pgrep -P '$pane_pid' 2>/dev/null | wc -l" || echo "0")
            if [ "$child_count" -eq 0 ]; then
                finished_sessions+=("$session")
            fi
        fi
    done

    if [ ${#finished_sessions[@]} -eq 0 ]; then
        echo "  No finished sessions to clean."
    else
        for session in "${finished_sessions[@]}"; do
            if [ "$DRY_RUN" = true ]; then
                echo "  Would kill session: $session"
            else
                echo "  Killing session: $session"
                ssh "$HOST" "tmux kill-session -t '$session'" || true
            fi
        done
    fi
    echo ""
fi

# Clean old archived log files
if [ "$CLEAN_LOGS" = true ]; then
    echo "Checking for archived logs older than $OLDER_THAN days on $HOST..."

    # Find archived log files (pattern: tmux-*.YYYYMMDD-HHMMSS.log)
    old_logs=$(ssh "$HOST" "find /tmp -maxdepth 1 -name 'tmux-*.????????-??????.log' -mtime +$OLDER_THAN 2>/dev/null" || true)

    if [ -z "$old_logs" ]; then
        echo "  No archived logs older than $OLDER_THAN days."
    else
        echo "$old_logs" | while read -r log; do
            if [ -n "$log" ]; then
                if [ "$DRY_RUN" = true ]; then
                    echo "  Would remove: $log"
                else
                    echo "  Removing: $log"
                    ssh "$HOST" "rm -f '$log'"
                fi
            fi
        done
    fi
    echo ""
fi

if [ "$DRY_RUN" = true ]; then
    echo "Run without --dry-run to actually perform cleanup."
else
    echo "✓ Cleanup complete."
fi
