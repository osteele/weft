# Claude Code channels

Weft can run as a Claude Code channel server. The channel sends project-scoped job updates into the current Claude Code session, so Claude can notice failed or completed jobs and suggest the next command.

## Install

Install the MCP server entry in Claude Code:

```bash
weft channel install
```

This updates `~/.claude.json` with:

```json
{
  "mcpServers": {
    "weft": {
      "command": "weft",
      "args": ["channel", "serve"]
    }
  }
}
```

During the Claude Code channel preview, registration alone is not enough. Start Claude Code with the development channel flag:

```bash
claude --dangerously-load-development-channels server:weft
```

To preview the config change without writing `~/.claude.json`:

```bash
weft channel install --dry-run
```

For other MCP clients, print a generic server entry:

```bash
weft channel install --target json
```

## Run manually

Claude Code normally starts the server from its MCP config. For debugging, you can run it directly from a project directory:

```bash
weft channel serve
```

The project defaults to the same current-directory resolution used by `weft project jobs`. To force a project:

```bash
weft channel serve --project augur
```

The server uses stdin/stdout for MCP JSON-RPC and writes diagnostics to stderr. Claude Code records stderr in its debug logs.

## What Claude receives

On startup, Weft sends one summary of unprocessed terminal jobs for the resolved project. Unprocessed means the job does not have the reserved `processed` tag. The summary uses the same 14-day terminal-job window as `weft narrate`.

Example startup message:

```text
augur has 3 unprocessed terminal jobs from the last 14 days: 1 completed, 2 failed. Review: weft jobs list --project augur --unprocessed --group-by status
```

After startup, Weft watches the jobs database and sends channel notifications for terminal job transitions:

- `completed`
- `failed`
- `dead`
- `killed`
- `canceled`

It also sends cloud instance lifecycle notifications for visible project instances entering `grace`, `completed`, `failed`, or `canceled`.

By default, job starts are not sent. Include running and starting transitions with:

```bash
weft channel serve --include-running
```

## Debounce

Transitions are debounced per job for two seconds by default. This coalesces rapid status churn for one job without merging unrelated jobs.

Change the debounce window:

```bash
weft channel serve --debounce 5s
```

Disable debounce for debugging:

```bash
weft channel serve --debounce 0s
```

## Troubleshooting

If Claude Code does not receive events:

1. Confirm `~/.claude.json` has the `weft` MCP server entry.
2. Start Claude Code with `--dangerously-load-development-channels server:weft`.
3. Run from a directory that resolves to a known Weft project, or pass `--project`.
4. Check Claude Code debug logs for `weft channel:` stderr messages.
