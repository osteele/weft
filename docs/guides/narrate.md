# Activity Narration (`weft narrate`)

`weft narrate` watches job, cloud-instance, campaign, and autopilot
transitions and streams a human-readable LLM-generated commentary to
stdout.

For the full map of Weft features that call Anthropic/OpenRouter or invoke
`claude -p`, see [LLM and Claude Integrations](llm-integrations.md).

It is for *situational awareness during active campaigns*, like a
colleague dictating what's happening over your shoulder. It is **not**
authoritative status — `weft instance watch`, `weft job watch`, and
`weft autopilot status` remain the structured source of truth.

## Quick start

```sh
export ANTHROPIC_API_KEY=sk-ant-...
weft narrate            # streams commentary indefinitely; Ctrl-C to stop
weft narrate --once     # one-shot description of current state
```

## What it watches

The command wakes when the local SQLite database changes, with a maximum
interval fallback (default 30s). After a DB-change wake it waits for a short
quiet window (default 5s) so a burst of job/instance updates produces one
narration pass instead of several.

On each narration pass, the command:

1. Reads the local SQLite DB for active jobs, non-terminal cloud
   instances, active campaigns, and autopilot state.
2. Computes a delta against the previous pass.
3. Sends a structured snapshot + delta to Anthropic Claude, which returns
   a narration paragraph and a terse state recap (via a tool call).
4. Prints the narration; appends the recap to a growing context block so
   the next pass has continuity.

**Ticks with no transitions are skipped — no API call, no token spend, no
output.** If you've launched the narrator and nothing has happened in the
DB since the priming snapshot, you'll see one paragraph and then silence
until something actually transitions. Use `--debug` to confirm change wakes
and max-interval passes are firing.

## Flags

| Flag | Default | Effect |
| --- | --- | --- |
| `--tick DURATION` | `30s` | Maximum interval between checks. |
| `--quiet-window DURATION` | `5s` | Quiet period after a DB change before narrating. |
| `--project NAME` | (whole system) | Limit to a project (uses the same project derivation as `weft project jobs`). |
| `--model NAME` | from config | Override the provider model ID. |
| `--once` | loop | Emit one narration of current state and exit. |
| `--debug` | off | Write raw delta payloads and per-tick `usage` (cache hit/miss counts) to stderr. |
| `--slack` | off | Also post emitted entries to the configured Slack webhook. |
| `--slack-min-interval DURATION` | `5m` | Minimum interval between Slack posts. |

## Configuration

`~/.config/weft/config.toml`:

```toml
[llm]
provider = "anthropic" # or "openrouter"
model = "claude-sonnet-4-20250514"

[ai.narrate]
tick_seconds = 30
quiet_seconds = 5
max_output_tokens = 1600
compaction_threshold_tokens = 15000
slack = false
slack_min_interval_seconds = 300
```

For OpenRouter:

```toml
[llm]
provider = "openrouter"
model = "anthropic/claude-sonnet-4.6"
```

API key resolution prefers environment variables over config. Anthropic uses
`ANTHROPIC_API_KEY`. OpenRouter uses `OPENROUTER_API_KEY`, with
`CLAUDE_OPENROUTER_API_KEY` and `CLAUDEM_OPENROUTER_API_KEY` accepted for
local compatibility. As a fallback, `[llm].api_key` may be set in
`config.toml`, or a matching `KEY=VALUE` entry may be placed in the legacy
`~/.config/weft/config` file.

If no key is found, the command prints a notice to stderr and exits 0
without making any API calls.

Slack posting uses the existing incoming-webhook configuration from
`WEFT_SLACK_WEBHOOK` or `SLACK_WEBHOOK=...` in `~/.config/weft/config`.
Incoming webhooks are bound to one Slack channel, so `weft narrate` does not
select arbitrary channels by name.

## Cost shape

Anthropic-compatible prompt caching is used aggressively. Direct Anthropic and
OpenRouter Claude models both receive explicit `cache_control` breakpoints:

- The system prompt + glossary are marked with a 5-minute ephemeral
  cache breakpoint. Stable across ticks.
- The accumulated prior-recap block is wrapped in a second cache
  breakpoint. It grows append-only tick-by-tick, so each tick's prefix
  is a longer match against the previous tick's cache entry.
- Only the volatile current-state JSON + delta is uncached on each call.

With 30s ticks, the cache stays warm indefinitely on providers that honor the
5-minute sliding TTL.

When the accumulated recap chain crosses
`compaction_threshold_tokens`, a one-shot compaction call rewrites the
chain into a single fresh "story so far" block. This eats one cache
miss, then clean hits resume.

Running `--debug` writes per-tick `cache_creation_input_tokens` and
`cache_read_input_tokens` to stderr. After tick 1, reads should
dominate creates by an order of magnitude or more — if not, the prefix
has been invalidated by something upstream.

## Risks

- **Hallucinated causality.** The model may invent reasons for
  transitions ("the job failed because…"). The system prompt requires
  hedging language ("likely", "possibly"), but treat the prose as
  commentary, not diagnosis.
- **Quiet-window latency.** DB-change driven; bursts are intentionally
  delayed until the quiet window expires, and the max interval still
  bounds checks if filesystem notifications are unavailable.
- **Register drift.** The terse recap chain is wrapped in a
  `<prior_state_recap>` block that the system prompt explicitly
  disclaims, so the model doesn't imitate it. If you ever see narration
  start writing in bullet points, the disclaim instruction has stopped
  landing — file a bug.

## Implementation pointers

- `cmd/narrate.go` — cobra command, orchestration loop.
- `internal/app/dbwatch` — DB/WAL/SHM change source shared with watch and
  autopilot loops.
- `internal/narrate/snapshot.go` — DB → `Snapshot` + `Delta` diffing.
- `internal/narrate/format.go` — snapshot/delta → JSON for the prompt.
- `internal/narrate/session.go` — recap accumulator + compaction trigger.
- `internal/narrate/client.go` — Anthropic/OpenRouter API client with
  explicit cache breakpoints and tool-use structured output.
- `internal/narrate/prompt.go` — system prompt, glossary, tool schema.
