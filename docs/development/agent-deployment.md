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

To converge more than one host, pass `--all` (every inventory host) or
`--hosts a,b`. Every named host is attempted even if an earlier one fails, and
a per-host summary is printed. A host that could not be reached is reported
separately from one that failed to converge, and the exit status is non-zero if
any host did not converge.

## Verify what is deployed

```bash
weft host agent-status            # cached observations, no SSH or R2 requests
weft host agent-status --json     # same, versioned schema
```

It prints, per inventory host, the desired version (computed from the local
source tree), the last verified deployed version, the latest runner identity,
the queue protocol version, and how old each observation is.

`STATUS` is three-valued, and the distinction is the point:

- `current` — desired, deployed, and running versions agree.
- `stale` — a version was observed and it differs from the desired build.
- `unknown` — nothing was observed, the observation is too old to trust, or the
  desired version could not be computed. An unobserved host is not a
  known-stale host, and the `DETAIL` column says which case applies.

**The agent version is a content hash over `cmd/agent`'s transitive source
closure** (`localAgentSourceVersion`, `internal/agentdeploy/version.go`), with
the jj or git commit id as a fallback. So changing any package the agent
imports — `internal/sync`, say — changes the desired version and triggers a
redeploy, even when the resulting binary is byte-identical.

**Do not verify a deployment by grepping the remote binary for strings.** Go's
linker eliminates code that is unreachable from the binary's entry point, so a
symbol can be absent from `weft-agent` purely because the agent never calls it,
regardless of which source built it. A string search therefore reports staleness
that is not there. Compare versions instead: `weft host agent-status`, or
`weft-agent --version` on the host against a local `weft build-agents
--from-source`.

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
