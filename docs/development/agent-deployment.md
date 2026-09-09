# Agent Deployment

The `weft-agent` binary runs on remote hosts (titan, atlas) and is auto-deployed
by `EnsureAgentUpToDate()` during TUI sync. To manually re-deploy, follow the
steps below.

## Cross-compile on studio

Always cross-compile on studio — it's ~5x faster than localhost:

```bash
# 1. Sync sources to studio
rsync -az --exclude .jj --exclude dist --exclude .git . studio:~/code/research-tools/weft/

# 2. Cross-compile on studio
ssh studio 'cd ~/code/research-tools/weft && GOOS=linux GOARCH=amd64 go build -o dist/weft-agent-linux-amd64 ./cmd/agent'

# 3. Copy back locally (for EnsureBuilt cache) and/or deploy to target host
scp studio:~/code/research-tools/weft/dist/weft-agent-linux-amd64 dist/weft-agent-linux-amd64
scp dist/weft-agent-linux-amd64 titan:~/.cache/weft/bin/weft-agent
ssh titan 'chmod +x ~/.cache/weft/bin/weft-agent'

# 4. Kill the runner session so it restarts with the new binary
ssh titan 'tmux kill-session -t weft-runner 2>/dev/null; true'
```

## Key files

- `internal/agentdeploy/deploy.go` — `EnsureAgentUpToDate`, deploys via scp + atomic rename
- `internal/agentdeploy/build.go` — `EnsureBuilt`, local cache at `~/Library/Caches/weft/builds/<version>/` on macOS (or platform `os.UserCacheDir()/weft/builds/<version>/`)
- `internal/agentdeploy/version.go` — `LocalAgentVersionForTarget`; source-checkout executables hash the agent inputs, while installed executables use the adjacent `.agent-version.json` identity for targets it records as prepared and use the source hash for other targets
- Remote binary path: `~/.cache/weft/bin/weft-agent`

## Background prewarm notes

`just build` and `just install` start a best-effort prewarm (`weft build-agents --targets linux-amd64 --from-source`) before the local build/install work, then wait for the prewarm before the recipe exits. This overlaps local work with agent preparation while ensuring chained commands do not race an old background `weft` process. After installing the CLI, `just install` runs the new binary with `--from-source --record-installed-identity`; a successful agent build records the executable-bound identity used outside the source checkout. If that identity has no cached binary for a requested target, Weft builds the target only when the current source has the same identity. Otherwise, rerun `just install` or use both `--from-source` and `--record-installed-identity` so the rebuilt agent and recorded identity change together.

The checkout binary and installed CLI intentionally have separate identities:
`./weft` follows the current source tree, while the installed CLI follows its
recorded prepared targets. Switching between them after agent source changes
can redeploy and restart an on-prem runner.

If you suspect prewarm/build issues, inspect:

```bash
tail -n 100 ~/.cache/weft/agent-prewarm.log
```
