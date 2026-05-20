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
- `internal/agentdeploy/build.go` — `EnsureBuilt`, local cache at `~/Library/Caches/weft/builds/<version>/` on macOS (or platform `os.UserCacheDir()/weft/builds/<version>/`)
- `internal/agentdeploy/version.go` — `LocalAgentVersion`, prefers a deterministic source hash (with VCS fallback)
- Remote binary path: `~/.cache/weft/bin/weft-agent`

## Background prewarm notes

`just build` and `just install` start a best-effort prewarm (`weft build-agents --targets linux-amd64`) before the local build/install work, then wait for the prewarm before the recipe exits. This overlaps local work with agent preparation while ensuring chained commands do not race an old background `weft` process.

If you suspect prewarm/build issues, inspect:

```bash
tail -n 100 ~/.cache/weft/agent-prewarm.log
```
