# Build Benchmarks

Timing data and analysis for weft build strategies (measured 2026-03-16).

## Agent Build Times

### linux/amd64

#### Compile-only times (build cache cleared, module cache warm)

| VM size | Cold compile | Warm incremental |
|---|---|---|
| shared-cpu-1x (1GB) | 1m39s | 2.1s |
| shared-cpu-2x (4GB) | 4m45s | 6.8s |
| **performance-2x** (4GB) | **55s** | **0.23s** |

Measured 2026-03-18. Cold = build cache wiped, module cache warm. Warm = touch
one `.go` file, rebuild. The shared-cpu-2x is slower than 1x due to CPU
throttling on sustained workloads (user CPU time is similar, but real time
balloons from scheduling delays). performance-2x uses dedicated cores and
parallelizes effectively (real < user on cold compile).

#### End-to-end pipeline times (measured 2026-03-16)

| Method | Warm | Cold |
|---|---|---|
| Fly builder (shared-cpu-1x) | 50–72s | ~1,664s (cold everything) / ~1,576s (warm modules) |
| Local cross-compile (macOS) | — failed | — |

Local macOS cross-compile for linux/amd64 with CGO fails: missing Linux sysroot
headers (`stdlib.h`, `pthread.h`, `grp.h`). CGO_ENABLED=0 builds would succeed
but omit NVML telemetry.

End-to-end includes: machine start, rsync restore, source sync, compile, binary
download, machine stop. Fly warm range: 50–52s (rsync from /data/bin, Go cache
warm) to ~72s (first run after machine restart when rsync not yet on /data).
Cold: ~1,576s compile + ~88s module download = ~1,664s.

Source transfer method: rsync over `flyctl proxy` SSH tunnel. First run installs
rsync via apt-get and copies binary + libpopt to `/data/bin` and `/data/lib` for
reuse on subsequent startups (Fly root filesystem is ephemeral; `/data` persists).

### darwin/arm64

| Method | Warm | Cold |
|---|---|---|
| Local native (on studio) | 0.17s compile | 26.39s compile |
| SSH to studio (`WEFT_MACOS_BUILDER_HOST`) | ~61s total (rsync + compile) | ~81s total |

Studio compile times are near-instant warm due to Go build cache. Total pipeline
time (61s warm) is dominated by rsync, not compilation.

Source tree synced: 3.66 MB total; incremental rsync: ~18 KB.

Notes:
- "Warm" = Go module cache and build cache populated
- "Cold" = empty caches

## CLI Build Times (weft binary)

| Scenario | Time |
|---|---|
| Warm (incremental) | ~96s |

Derived from serial build measurement: agents (~58s) + CLI = ~154s serial total,
so CLI ≈ 96s. Cold CLI build not measured.

## Serial vs Parallel Build

Old approach (serial — `//go:embed` forced dependency):
- `build-agents` must complete before `go build .` can start (Go reads embedded files at compile time)
- Warm total: ~58s agents + ~96s weft CLI = **~154s**

New approach (parallel — filesystem fallback removes embed dependency):
- `build-agents` and `go build .` run concurrently
- Warm total: max(~58s, ~96s) = **~96s** (dominated by weft CLI)
- linux/amd64 skipped (no Fly vars): max(~61s studio arm64, ~96s CLI) = **~96s**

## xdelta3 Differential Download Analysis

Evaluated for linux/amd64 binary transfer from Fly builder.

| Metric | Value |
|---|---|
| Full binary size | 15 MB |
| Compressed xdelta3 patch | 1.4 MB (10x reduction) |
| Plain sftp download | ~17s |
| xdelta3 differential download | ~22s |

**Conclusion**: xdelta3 is not beneficial on the Fly path. SSH console overhead
(~18s baseline) makes xdelta3 slower (22s) than plain transfer (17s) despite
the 10x size reduction.

Studio copy-back timings (259s / 246s) were anomalous due to network instability
and are not representative.

## Build Strategy

The `just build` recipe runs `build-agents` and `go build .` in parallel via
background `&` with `wait`. Since agent binaries are no longer embedded (no
`//go:embed`), the weft CLI no longer has a compile-time dependency on the
binaries directory.

### Environment Variables

| Variable | Purpose |
|---|---|
| `WEFT_FLY_BUILDER_APP` | Fly.io app name for linux/amd64 cross-compilation |
| `WEFT_FLY_BUILDER_MACHINE` | Fly.io machine ID |
| `WEFT_FLY_BUILDER_BASE` | Remote base dir (default: `/data/weft-builder`) |
| `WEFT_FLY_BUILDER_GO_BIN` | Go binary path on Fly machine (default: `/usr/local/go/bin/go`) |
| `WEFT_FLY_BUILDER_PROXY_PORT` | Local SSH proxy port for rsync (default: `2222`) |
| `WEFT_MACOS_BUILDER_HOST` | SSH host for darwin/arm64 builds (e.g., `studio`) |
| `WEFT_MACOS_BUILDER_DIR` | Remote dir on macOS builder (default: `~/.cache/weft/agent-build`) |

Without `WEFT_FLY_BUILDER_APP`/`MACHINE`, the linux/amd64 agent is skipped with
an info message. Without `WEFT_MACOS_BUILDER_HOST`, darwin/arm64 is built locally.
