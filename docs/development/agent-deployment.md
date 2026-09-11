# Agent Deployment

The `weft-agent` binary runs on on-prem queue-runner hosts and cloud
instances. Deployment is normally automatic: `EnsureAgentUpToDate`
(`internal/agentdeploy/deploy.go`) compares local and remote agent versions
during source sync and job dispatch, deploys a matching build, and the queue
runner re-execs into the new binary while preserving pending and running job
state.

## Deploy to an edge host

weft's CLI deploys to edge hosts directly:

```bash
just deploy-agent <host>     # or: weft queue update <host>
```

`weft queue update <host>`:

1. Deploys the current agent binary if the remote version differs
   (`EnsureAgentUpToDateWithOptions`), verifying the deployed binary by
   fingerprint before recording the deployment.
2. Starts the queue runner if it is not running; otherwise sends a restart op
   so the running process re-execs into the new binary — the tmux session,
   pending jobs, and the currently running job all survive.

Hosts must be in the local inventory first (`weft host discover <hostname>`).

## SSH accounts

Hosts without an `ssh_user` override in `~/.config/weft/config.toml` resolve
through the user's `~/.ssh/config` entry for the host name. Give a host a
dedicated worker account with:

```toml
[hosts.studio]
ssh_user = "agent"
ssh_identity_file = "~/.ssh/agent_studio_ed25519"
```

Deploys always target the configured worker account and never the personal
login on the same host. studio is configured this way: weft operations target
`agent@studio` with a dedicated identity file, never the personal account.

## Key files

- `internal/agentdeploy/deploy.go` — `EnsureAgentUpToDate`: compare, deploy,
  verify.
- `internal/agentdeploy/build.go` — `EnsureBuilt`, local build cache at
  `~/Library/Caches/weft/builds/<version>/` (platform `os.UserCacheDir()`).
- `internal/agentdeploy/version.go` — `LocalAgentVersionForTarget`;
  source-checkout executables hash the agent inputs, while installed
  executables use the adjacent `.agent-version.json` identity for targets
  recorded as prepared.
- Remote binary path: `~/.cache/weft/bin/weft-agent`.

## Building agent binaries

`just build` and `just install` rebuild the agent binaries: a background
prewarm runs `weft build-agents --targets linux-amd64 --from-source`, and the
recipe waits for it before building the CLI. Plain `go build` does NOT rebuild
the embedded agent binaries — deploying a stale agent produces an error.

If no cached binary exists for a host's platform, deployment falls back to a
native build on the host itself (`BuildOnHostWithProgress`).

The checkout binary and installed CLI carry separate identities: `./weft`
follows the current source tree, while the installed CLI follows its recorded
prepared targets. Switching between them after agent source changes can
redeploy and restart an on-prem runner.

If you suspect prewarm/build issues:

```bash
tail -n 100 ~/.cache/weft/agent-prewarm.log
```
