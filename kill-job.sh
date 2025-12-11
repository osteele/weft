#!/bin/bash
#
# Kill a persistent tmux session on remote host
#
# Usage:
#   kill-job.sh <host> <session-name>
#

set -euo pipefail

if [ $# -lt 2 ]; then
    echo "Usage: $0 <host> <session-name>"
    echo ""
    echo "Example:"
    echo "  $0 cool30 train-gpt2"
    exit 1
fi

HOST="$1"
SESSION_NAME="$2"

echo "Killing session '$SESSION_NAME' on $HOST..."
ssh "$HOST" "tmux kill-session -t '$SESSION_NAME'"
echo "✓ Session killed"
