#!/bin/bash
#
# Queue a job for later execution (saves to pending without trying to start)
#
# Usage:
#   queue-job.sh [options] <host> <session-name> <working-dir> <command...>
#
# Options:
#   -d, --description TEXT  Description of the job
#
# The job will be saved as pending and can be started later with:
#   retry-job.sh <job-id>
#   retry-job.sh --all
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/db.sh"

show_usage() {
    echo "Usage: $0 [options] <host> <session-name> <working-dir> <command...>"
    echo ""
    echo "Queue a job for later execution without starting it now."
    echo ""
    echo "Options:"
    echo "  -d, --description TEXT  Description of the job"
    echo ""
    echo "Examples:"
    echo "  $0 cool30 train-gpt2 /mnt/code/LM2 'python train.py'"
    echo "  $0 -d 'Training run' cool30 train /mnt/code/LM2 'python train.py'"
    echo ""
    echo "Later, start the job with:"
    echo "  retry-job.sh <job-id>"
    echo "  retry-job.sh --all"
}

# Parse optional flags
DESCRIPTION=""
while [ $# -gt 0 ]; do
    case "$1" in
        -d|--description)
            DESCRIPTION="$2"
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
            break
            ;;
    esac
done

if [ $# -lt 4 ]; then
    show_usage
    exit 1
fi

HOST="$1"
SESSION_NAME="$2"
WORKING_DIR="$3"
shift 3
COMMAND="$*"

echo "Queuing job for later execution:"
echo "  Host: $HOST"
echo "  Session: $SESSION_NAME"
echo "  Working dir: $WORKING_DIR"
echo "  Command: $COMMAND"
if [ -n "$DESCRIPTION" ]; then
    echo "  Description: $DESCRIPTION"
fi
echo ""

JOB_ID=$(db_record_pending "$HOST" "$SESSION_NAME" "$WORKING_DIR" "$COMMAND" "$DESCRIPTION")
echo "Job queued with ID: $JOB_ID"
echo ""
echo "To start this job later:"
echo "  $SCRIPT_DIR/retry-job.sh $JOB_ID"
echo ""
echo "To see all pending jobs:"
echo "  $SCRIPT_DIR/retry-job.sh --list"
