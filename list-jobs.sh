#!/bin/bash
#
# List and query jobs from the local database
#
# Usage:
#   list-jobs.sh [options]
#
# Options:
#   --running        Show only running jobs
#   --completed      Show only completed jobs
#   --dead           Show only dead jobs
#   --host HOST      Filter by host
#   --search QUERY   Search by description or command
#   --limit N        Limit results (default: 50)
#   --show ID        Show detailed info for job ID
#   --cleanup DAYS   Delete jobs older than N days
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/db.sh"

show_usage() {
    echo "Usage: $0 [options]"
    echo ""
    echo "List and query jobs from the local database."
    echo ""
    echo "Options:"
    echo "  --running        Show only running jobs"
    echo "  --completed      Show only completed jobs"
    echo "  --dead           Show only dead jobs"
    echo "  --pending        Show only pending jobs (not yet started)"
    echo "  --host HOST      Filter by host"
    echo "  --search QUERY   Search by description or command"
    echo "  --limit N        Limit results (default: 50)"
    echo "  --show ID        Show detailed info for a specific job"
    echo "  --cleanup DAYS   Delete completed jobs older than DAYS"
    echo ""
    echo "Examples:"
    echo "  $0                        # List recent jobs"
    echo "  $0 --running              # List running jobs"
    echo "  $0 --pending              # List pending jobs"
    echo "  $0 --host cool30          # List jobs on cool30"
    echo "  $0 --search 'training'    # Search for jobs"
    echo "  $0 --show 42              # Show details for job #42"
    echo "  $0 --cleanup 30           # Remove jobs older than 30 days"
}

STATUS=""
HOST=""
SEARCH=""
LIMIT=50
SHOW_ID=""
CLEANUP_DAYS=""

while [ $# -gt 0 ]; do
    case "$1" in
        --running)
            STATUS="running"
            shift
            ;;
        --completed)
            STATUS="completed"
            shift
            ;;
        --dead)
            STATUS="dead"
            shift
            ;;
        --pending)
            STATUS="pending"
            shift
            ;;
        --host)
            HOST="$2"
            shift 2
            ;;
        --search)
            SEARCH="$2"
            shift 2
            ;;
        --limit)
            LIMIT="$2"
            shift 2
            ;;
        --show)
            SHOW_ID="$2"
            shift 2
            ;;
        --cleanup)
            CLEANUP_DAYS="$2"
            shift 2
            ;;
        -h|--help)
            show_usage
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            show_usage
            exit 1
            ;;
    esac
done

# Handle cleanup mode
if [ -n "$CLEANUP_DAYS" ]; then
    db_cleanup_old "$CLEANUP_DAYS"
    exit 0
fi

# Handle show mode
if [ -n "$SHOW_ID" ]; then
    result=$(db_get_job_by_id "$SHOW_ID")
    if [ -z "$result" ]; then
        echo "Job not found: $SHOW_ID"
        exit 1
    fi

    # Parse tab-separated result
    IFS=$'\t' read -r id host session_name working_dir command description start_time end_time exit_code status <<< "$result"

    echo "ID: $id"
    echo "Host: $host"
    echo "Session: $session_name"
    echo "Working Dir: $working_dir"
    echo "Command: $command"
    echo "Description: ${description:-(none)}"
    echo "Started: $(date -r "$start_time" '+%Y-%m-%d %H:%M:%S' 2>/dev/null || date -d "@$start_time" '+%Y-%m-%d %H:%M:%S')"
    if [ -n "$end_time" ] && [ "$end_time" != "" ]; then
        echo "Ended: $(date -r "$end_time" '+%Y-%m-%d %H:%M:%S' 2>/dev/null || date -d "@$end_time" '+%Y-%m-%d %H:%M:%S')"
        duration=$((end_time - start_time))
        hours=$((duration / 3600))
        minutes=$(( (duration % 3600) / 60 ))
        seconds=$((duration % 60))
        printf "Duration: %02d:%02d:%02d\n" "$hours" "$minutes" "$seconds"
    else
        echo "Ended: (still running)"
    fi
    echo "Exit Code: ${exit_code:--}"
    echo "Status: $status"
    exit 0
fi

# Handle search mode
if [ -n "$SEARCH" ]; then
    db_search_jobs "$SEARCH" "$LIMIT"
    exit 0
fi

# Regular list mode
db_list_jobs "$STATUS" "$HOST" "$LIMIT"
