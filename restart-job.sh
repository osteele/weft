#!/bin/bash
#
# Restart a job using its saved metadata
#
# Usage:
#   restart-job.sh <host> <session-name>
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ $# -lt 2 ]; then
    echo "Usage: $0 <host> <session-name>"
    echo ""
    echo "Restarts a job using the saved metadata from when it was originally started."
    echo ""
    echo "Example:"
    echo "  $0 cool30 train-gpt2"
    exit 1
fi

HOST="$1"
SESSION_NAME="$2"

METADATA_FILE="/tmp/tmux-${SESSION_NAME}.meta"

# Check if metadata file exists
if ! ssh "$HOST" "test -f '$METADATA_FILE'"; then
    echo "ERROR: Metadata file not found: $METADATA_FILE"
    echo "Cannot restart job without metadata. The job may have been started before"
    echo "metadata tracking was added, or the metadata was cleaned up."
    exit 1
fi

# Read metadata
echo "Reading metadata for '$SESSION_NAME'..."
metadata=$(ssh "$HOST" "cat '$METADATA_FILE'")

working_dir=$(echo "$metadata" | grep '^working_dir=' | cut -d= -f2-)
command=$(echo "$metadata" | grep '^command=' | cut -d= -f2-)

if [ -z "$working_dir" ] || [ -z "$command" ]; then
    echo "ERROR: Invalid metadata file. Missing working_dir or command."
    exit 1
fi

echo "  Working directory: $working_dir"
echo "  Command: $command"
echo ""

# Kill existing session if running
if ssh "$HOST" "tmux has-session -t '$SESSION_NAME' 2>/dev/null"; then
    echo "Killing existing session '$SESSION_NAME'..."
    ssh "$HOST" "tmux kill-session -t '$SESSION_NAME'"
fi

# Use run-remote.sh to start the new session
echo "Starting new session..."
"$SCRIPT_DIR/run-remote.sh" "$HOST" "$SESSION_NAME" "$working_dir" $command
