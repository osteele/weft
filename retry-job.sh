#!/bin/bash
#
# Retry a pending job that couldn't be started
#
# Usage:
#   retry-job.sh <job-id> [--host <new-host>]
#   retry-job.sh --list [--host <host>]
#   retry-job.sh --all [--host <host>]
#
# Options:
#   --host HOST   Run on a different host than originally specified
#   --list        List all pending jobs
#   --all         Retry all pending jobs (optionally for a specific host)
#   --delete ID   Delete a pending job without running it
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/db.sh"

show_usage() {
    echo "Usage: $0 <job-id> [--host <new-host>]"
    echo "       $0 --list [--host <host>]"
    echo "       $0 --all [--host <host>]"
    echo "       $0 --delete <job-id>"
    echo ""
    echo "Retry pending jobs that couldn't be started (e.g., due to connection failures)."
    echo ""
    echo "Options:"
    echo "  <job-id>          The ID of the pending job to retry"
    echo "  --host HOST       Run on a different host than originally specified"
    echo "  --list            List all pending jobs"
    echo "  --all             Retry all pending jobs"
    echo "  --delete ID       Delete a pending job without running it"
    echo ""
    echo "Examples:"
    echo "  $0 --list                    # Show pending jobs"
    echo "  $0 42                        # Retry job #42"
    echo "  $0 42 --host studio          # Retry job #42 on 'studio' instead"
    echo "  $0 --all                     # Retry all pending jobs"
    echo "  $0 --all --host cool30       # Retry all pending jobs for cool30"
    echo "  $0 --delete 42               # Remove pending job without running"
}

LIST_MODE=false
ALL_MODE=false
DELETE_MODE=false
JOB_ID=""
NEW_HOST=""
FILTER_HOST=""

while [ $# -gt 0 ]; do
    case "$1" in
        --list)
            LIST_MODE=true
            shift
            ;;
        --all)
            ALL_MODE=true
            shift
            ;;
        --delete)
            DELETE_MODE=true
            JOB_ID="$2"
            shift 2
            ;;
        --host)
            if [ "$LIST_MODE" = true ] || [ "$ALL_MODE" = true ]; then
                FILTER_HOST="$2"
            else
                NEW_HOST="$2"
            fi
            shift 2
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
            JOB_ID="$1"
            shift
            ;;
    esac
done

# List mode
if [ "$LIST_MODE" = true ]; then
    echo "Pending jobs:"
    echo ""
    db_list_pending "$FILTER_HOST"
    exit 0
fi

# Delete mode
if [ "$DELETE_MODE" = true ]; then
    if [ -z "$JOB_ID" ]; then
        echo "Error: --delete requires a job ID"
        show_usage
        exit 1
    fi

    job_info=$(db_get_pending_job "$JOB_ID")
    if [ -z "$job_info" ]; then
        echo "Error: Pending job not found: $JOB_ID"
        exit 1
    fi

    db_delete_pending "$JOB_ID"
    echo "Deleted pending job #$JOB_ID"
    exit 0
fi

# All mode - retry all pending jobs
if [ "$ALL_MODE" = true ]; then
    pending_ids=$(sqlite3 "$DB_FILE" "SELECT id FROM jobs WHERE status = 'pending' $([ -n "$FILTER_HOST" ] && echo "AND host = '$FILTER_HOST'")" 2>/dev/null || echo "")

    if [ -z "$pending_ids" ]; then
        echo "No pending jobs found"
        exit 0
    fi

    success_count=0
    fail_count=0

    for id in $pending_ids; do
        echo "Retrying job #$id..."
        if "$0" "$id"; then
            ((success_count++)) || true
        else
            ((fail_count++)) || true
        fi
        echo ""
    done

    echo "Summary: $success_count succeeded, $fail_count failed"
    exit 0
fi

# Single job retry
if [ -z "$JOB_ID" ]; then
    show_usage
    exit 1
fi

# Get job info
job_info=$(db_get_pending_job "$JOB_ID")
if [ -z "$job_info" ]; then
    echo "Error: Pending job not found: $JOB_ID"
    echo ""
    echo "Use --list to see pending jobs"
    exit 1
fi

# Parse job info (tab-separated: id|host|session_name|working_dir|command|description|start_time|end_time|exit_code|status)
IFS=$'\t' read -r id host session_name working_dir command description start_time end_time exit_code job_status <<< "$job_info"

# Use new host if specified
if [ -n "$NEW_HOST" ]; then
    echo "Retrying on different host: $NEW_HOST (originally: $host)"
    host="$NEW_HOST"
fi

echo "Retrying job #$JOB_ID:"
echo "  Host: $host"
echo "  Session: $session_name"
echo "  Working dir: $working_dir"
echo "  Command: $command"
if [ -n "$description" ]; then
    echo "  Description: $description"
fi
echo ""

# Delete the pending job entry
db_delete_pending "$JOB_ID"

# Run the job using run-remote.sh
if [ -n "$description" ]; then
    "$SCRIPT_DIR/run-remote.sh" -d "$description" "$host" "$session_name" "$working_dir" "$command"
else
    "$SCRIPT_DIR/run-remote.sh" "$host" "$session_name" "$working_dir" "$command"
fi
