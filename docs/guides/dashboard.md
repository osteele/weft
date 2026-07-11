# Dashboard

`weft dashboard` is a tabbed at-a-glance view of the system: status counts,
running jobs, cloud fleet, alerts, and history. It complements `weft tui` —
the latter is the primary interface for managing and acting on jobs; the
dashboard is optimized for monitoring and noticing problems.

```
weft dashboard                       # launch (lands on Pulse)
weft dashboard --start-tab fleet     # start on a specific tab
weft dashboard --cycle 15s           # start with auto-rotation on
```

`--start-tab` accepts a number (`1`–`9` or `0`) or a name (`pulse`, `timeline`,
`fleet`, `focus`, `tree`, `alerts`, `history`, `flow`, `cost`, `usage`).

## Views

| # | Tab      | What it shows                                                                |
| - | -------- | ---------------------------------------------------------------------------- |
| 1 | Pulse    | Status donut + sparklines for queue depth, running, $/hr, and failures       |
| 2 | Timeline | 6-hour Gantt of running and queued jobs, with a "now" line and real ETA      |
| 3 | Fleet    | Card grid of on-prem hosts and **live** cloud instances                      |
| 4 | Focus    | Full-width cards for each running job — progress, host, GPU, elapsed, ETA, cost |
| 5 | Tree     | Jobs grouped by project, then experiment id                                  |
| 6 | Alerts   | Anomaly-first: stuck-queued, clustered failures, over-target spend, paused   |
| 7 | History  | Last 23h stacked outcome bar + per-provider × hour heatmap of cloud results  |
| 8 | Flow     | Sankey-style: status → destination (host/instance) → provider, with $/hr     |
| 9 | Cost     | Spend rate, daily/monthly projection, sparkline, top spenders, per-provider, LLM API spend |
| 0 | Usage    | LLM call counts and tokens by feature/model (Anthropic), 30-day spend sparkline |

Each tab is fed from the same in-memory snapshot, which is refreshed every
few seconds (slowed when the window is unfocused). Tabs with attention
(currently: Alerts, when it has unresolved items) render in red on the tab
strip.

### What counts as a "live" cloud instance

The Fleet view and the header's `$/hr` figure include only launches whose
status is one of `launching`, `running`, `paused`, or `grace` — i.e. instances
that the cloud provider has actually been told to create, and that may be
billing. Launches in status `planned` (a scheduling intent that never reached
the provider) are excluded from both the count and the cost, and surfaced
instead as an entry in Alerts when they age past a day. Terminal statuses
(`completed`, `failed`, `canceled`) are excluded entirely.

This matches the spend shown on the provider dashboard. If the values differ,
the discrepancy is usually stale `planned` rows in the local DB — see the
Alerts tab.

## Keys

| Key            | Action                                                            |
| -------------- | ----------------------------------------------------------------- |
| `1` … `9`, `0` | Switch directly to the numbered tab (`0` = Usage)                  |
| `Tab` / `⇧Tab` | Next / previous tab                                                |
| `?`            | Toggle help overlay (`Esc` also closes)                            |
| `c`            | Toggle auto-cycle (rotates tabs at the configured interval)        |
| `+` / `-`      | Faster / slower cycle interval (5s … 120s ladder)                  |
| `p`            | Pause / resume autopilot (the only write action)                   |
| `r`            | Force refresh now (don't wait for the next tick)                   |
| `q`, `Ctrl-C`  | Quit                                                              |
| `Ctrl-Z`       | Suspend (resume with `fg`)                                         |

When the help overlay is open, tab keys are swallowed — close help first.
Pressing any tab-switch key while cycling resets the cycle timer, so the tab
you just selected doesn't immediately get rotated past.

## Cycle Mode

Use cycle mode to leave the dashboard on a second monitor and have it walk
through tabs on its own. Press `c` to toggle; `+` and `-` step through the
interval ladder. The active interval is shown on the right side of the tab
strip.

## Autopilot Pause / Resume

`p` toggles the autopilot pause flag. Reason is recorded as `"dashboard"` so
you can spot it in `weft autopilot status`. Resume with `p` again (or from
the CLI with `weft autopilot resume`).

This is the only write the dashboard performs. Killing, requeueing, and other
actions stay in `weft tui` and the CLI.

## Alerts

Each alert carries a timestamp (relative + clock time) so you can tell at a
glance whether something is current or stale. Warnings sort above informational
items, and within each severity newest first. Current alert types:

- **Spend over target** — current $/hr exceeds the configured soft target.
- **Stuck queued jobs** — a job has been queued without placement for over 30
  minutes.
- **Clustered failures** — two or more failed instances on the same provider
  in the last 24 hours.
- **Recent job failures** — count of jobs that failed in the last hour.
- **Stuck planned launches** — `planned` launches in the local DB that are
  more than a day old and were never created on the provider; usually
  indicates a cleanup pass is needed.
- **Autopilot paused** — surfaces who paused it and the recorded reason.

## LLM API Usage

The Usage tab and the LLM section of the Cost tab read from the `llm_calls`
table, which is populated automatically every time `weft narrate` calls
Anthropic (both narration and recap compaction). Each row records:

- `feature` — `narrate` or `compact`
- `model` — the exact model id sent to Anthropic
- input/output/cache-creation/cache-read token counts
- latency (ms) and cost (micro-USD) computed from the model's published price
- the job id, if the call was made on behalf of one
- an error message, if the call failed

OpenRouter calls are not recorded yet; rows will appear there once that path
is wired through the same `OnUsage` callback.

This tab does not count local Claude Code CLI usage from TUI AI assist,
automatic remediation coding agents, or Slack log summaries. See
[LLM and Claude Integrations](llm-integrations.md) for the feature-by-feature
call map.

Anthropic pricing lives in `internal/llmusage/pricing.go`. Update that table
when Anthropic publishes new prices; only new calls are affected — historical
rows keep the cost they were recorded with.

## Usage Log

The dashboard appends NDJSON to `~/.cache/weft/dashboard-usage.log`:

```
{"event":"session_start","ts":"…","pid":12345}
{"event":"tab_enter","tab":"Pulse","ts":"…","focused":true,"focus_observed":false}
{"event":"focus_change","ts":"…","focused":false,"focus_observed":true}
{"event":"tab_exit","tab":"Pulse","ts":"…","dwell_ms":33000,"focused":true,"focus_observed":true,"reason":"tab_switch"}
```

Dwell time accumulates only while the dashboard window is focused, so leaving
it open in a background tab does not inflate the numbers.

Every record carries a `focus_observed` field. The first focus or blur event
in a session flips it to `true`; if it stays `false` for the whole session,
your terminal doesn't report focus changes — focus-time analysis should
filter those sessions out. There is no in-UI indicator for this; it is logged
only.

Modern terminals (iTerm2, kitty, alacritty, recent xterm, tmux ≥ 1.9) report
focus. Bare `screen` sessions and some older terminals do not.

## Relationship to `weft tui`

`weft tui` remains the primary interactive interface for managing jobs. The
dashboard is focused on monitoring — it shows what `weft tui` shows plus
several at-a-glance views, and intentionally limits writes to a single action
(autopilot pause/resume). Use the two together: dashboard to notice
something, `weft tui` (or the CLI) to act on it.
