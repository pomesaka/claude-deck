# claude-deck plugin (marketplace)

This directory holds a shell-hook Claude Code plugin that appends session events (SessionStart, SessionEnd, Notification, Stop) to `~/.local/share/claude-deck/claude-deck-events.jsonl` with `jq`.

claude-deck does not read that file. Status and `/clear` tracking come from the `deck-status` plugin in [`deckmod/`](../deckmod), which is embedded in the claude-deck binary and passed to every session it launches with `--plugin-dir`. Installing this plugin has no effect on claude-deck. See [docs/hooks.md](../docs/hooks.md).
