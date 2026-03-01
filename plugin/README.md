# claude-deck plugin

Claude Code plugin for [claude-deck](../README.md) — a TUI dashboard for managing multiple Claude Code sessions.

This plugin registers hooks that report session lifecycle events (start, end, notification, stop) to claude-deck's event file. This enables real-time session status tracking in the dashboard.

## Install

```
/plugin marketplace add pomesaka/claude-deck
/plugin install claude-deck
```

For local development:

```bash
claude --plugin-dir /path/to/claude-deck/plugin
```

## What it does

The plugin registers hooks for these Claude Code events:

| Event | Purpose |
|-------|---------|
| `SessionStart` | Track new sessions and `/clear` transitions |
| `SessionEnd` | Track session termination and `/clear` |
| `Notification` | Detect approval/answer prompts (permission_prompt, elicitation_dialog, idle_prompt) |
| `Stop` | Detect when Claude finishes responding |

Events are written as JSONL to `~/.local/share/claude-deck/claude-deck-events.jsonl`.

## Requirements

- `jq` must be available in PATH
