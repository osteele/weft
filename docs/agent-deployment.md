# Agent Deployment

The `weft-agent` binary runs on remote hosts (titan, atlas) and is auto-deployed
by `EnsureAgentUpToDate()` during TUI sync. To manually re-deploy, follow the
steps below.

## Cross-compile on studio

Always cross-compile on studio — it's ~5x faster than localhost:

```bash
# 1. Sync sources to studio
rsync -az --exclude .jj --exclude dist --exclude .git . studio:~/code/utils/weft/

# 2. Cross-compile on studio
ssh studio 'cd ~/code/utils/weft && GOOS=linux GOARCH=amd64 go build -o dist/weft-agent-linux-amd64 ./cmd/agent'

# 3. Copy back locally (for EnsureBuilt cache) and/or deploy to target host
scp studio:~/code/utils/weft/dist/weft-agent-linux-amd64 dist/weft-agent-linux-amd64
scp dist/weft-agent-linux-amd64 titan:~/.cache/weft/bin/weft-agent
ssh titan 'chmod +x ~/.cache/weft/bin/weft-agent'

# 4. Kill the runner session so it restarts with the new binary
ssh titan 'tmux kill-session -t weft-runner 2>/dev/null; true'
```

## Key files

- `internal/agentdeploy/deploy.go` — `EnsureAgentUpToDate`, deploys via scp + atomic rename
- `internal/agentdeploy/build.go` — `EnsureBuilt`, local cache at `~/.cache/weft/builds/<version>/`
- `internal/agentdeploy/version.go` — `LocalAgentVersion`, uses jj commit hash of agent source files
- Remote binary path: `~/.cache/weft/bin/weft-agent`
