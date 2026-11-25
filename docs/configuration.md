# Configuration

Config file: `$XDG_CONFIG_HOME/claude-deck/config.toml` (`~/.config/claude-deck/config.toml` when `XDG_CONFIG_HOME` is unset)

All sections are optional. Unspecified values use built-in defaults.

```toml
# Data directory. See "Data directories" below for the default.
data_dir = "/path/to/claude-deck-data"

[defaults]
# Passed to every session as --permission-mode. Unset (the default): no flag,
# so Claude Code's own permissions.defaultMode in settings.json decides.
permission_mode = "plan"

[ghostty]
command = "ghostty"
# Width in pixels of the claude-deck pane when it runs inside Ghostty.
# The right split attaches to the tmux session.
deck_width = 400

[tmux]
command = "tmux"
# Letters, digits, "-" and "_". Default: "claude-deck" ("claude-deck-codex" with provider = "codex").
# Use a different name for each data directory: on start, the TUI closes the
# windows of this tmux session that its own data directory does not know.
session_name = "claude-deck"

[keybinds]
# Only these two keys can be rebound.
fork = "f"
open_term = "t"

[theme]
primary = "#7C3AED"
secondary = "#06B6D4"
success = "#10B981"
warning = "#F59E0B"
danger = "#EF4444"
bg_selected = "#313244"
border_focus = "#7C3AED"
text = "#CDD6F4"
text_dim = "#6C7086"
status_idle = "#808898"
status_subagent = "#60A5FA"
status_attention = "#C08552"
status_done = "#333346"
diff_add = "#A6E3A1"
diff_del = "#F38BA8"

[commands]
claude = "claude"
codex = "codex"
jj = "jj"

[runtime]
# "claude" (default) or "codex"
provider = "claude"

[discovery]
# Marker files to detect subprojects within jj repositories (monorepo support).
# Empty = repo root only (default).
project_markers = ["go.mod", "package.json", "Cargo.toml"]
excludes = ["Library", ".cache", "node_modules", ".git"]

[session]
# Finished sessions beyond this count are pruned, oldest first, along with their workspaces.
max_sessions = 30
max_jsonl_entries = 500
# Discovery reads transcripts modified within this many days.
discovery_days = 14
refresh_interval = "5s"

[projects."/Users/you/your-repo"]
# Untracked files to symlink into each new jj workspace.
workspace_symlinks = [".env", ".env.local", "secrets/"]
# Extra directories passed to Claude Code as --add-dir.
add_dirs = ["../shared-lib", "/absolute/path/to/docs"]
# Extra arguments passed to Claude Code as they are.
claude_args = ["--dangerously-load-development-channels", "server:xwb"]
```

## Runtime provider

- `claude`: status and `/clear` are reported by the deck-status plugin. It is embedded in the binary, written to `plugin/` in the data directory on startup, and passed to every Claude Code session claude-deck launches with `--plugin-dir`. No separate install is needed. Transcripts are read from `~/.claude/projects`.
- `codex`: Codex has no hooks. claude-deck reads the status from the Codex JSONL (`~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`) and writes it to its store, so the status is not updated while the TUI is not running. Only running and idle are shown; waiting for approval or an answer is not.
- With `codex`, unless `data_dir` and `[tmux] session_name` are set explicitly, the data directory gets a `-codex` suffix and the tmux session is `claude-deck-codex`. Each provider has its own `deck.db`.

## Telemetry attributes

Every session claude-deck starts gets `OTEL_RESOURCE_ATTRIBUTES` with `deck.session_id`, `deck.name` (the session name, such as `maika-7c78`) and `deck.repo` (the last element of the repository path). If you turn on Claude Code's OpenTelemetry export, its events carry these, so a collector can filter them by session. Attributes claude-deck itself was started with are kept in front, except `deck.*` ones, which a session that runs `claude-deck new` passes down. The alias is not included, because it can change while the session runs; look it up by `deck.session_id` with `claude-deck list`.

## Project discovery

- With `project_markers` empty (the default), only repository roots are offered.
- With `project_markers` set, claude-deck searches each repository with `fd` and also offers the directories that contain a marker. The repository root is always offered.
- A nested repository with its own `.jj` is not a subproject of the outer one. It is offered as a repository of its own.
- The search starts when the TUI starts. `n` shows the last result at once and searches again in the background; the list is updated in place, keeping the filter text and the selection.

## Workspace symlinks

`jj workspace add` does not copy untracked files such as `.env` into the new workspace. `workspace_symlinks` lists files and directories to symlink from the repository root instead.

- Paths are relative to the repository root.
- A missing source is skipped, so the same list works for repositories without `.env`.
- An existing destination is skipped (for example, a file jj already checked out).
- Absolute paths and paths containing `..` are ignored.

## Additional directories

`add_dirs` is passed as `--add-dir` when claude-deck creates, resumes or forks a session in that repository. Relative paths are resolved from the repository root. Absolute paths are used as-is.

## Claude Code arguments

`claude_args` is appended to the `claude` command line when claude-deck creates, resumes or forks a session in that repository. Use it for flags that can only be given at startup. It is not passed to Codex.

```bash
claude-deck project args ~/your-repo -- --dangerously-load-development-channels server:xwb   # set
claude-deck project args ~/your-repo           # show
claude-deck project args --clear ~/your-repo   # remove
claude-deck project list                       # every project's settings
```

- The command rewrites only the `claude_args` line of `config.toml`. Comments and the other settings are kept.
- A change applies from the next session started, without restarting the TUI. Sessions that are already running keep the arguments they were started with.
- The directory is resolved to its repository root, the same way as `claude-deck new --dir`.

## Data directories

The data directory is, in order of precedence:

1. `$CLAUDE_DECK_DATA_DIR`
2. `data_dir` in `config.toml`
3. `$XDG_DATA_HOME/claude-deck`, or `~/.local/share/claude-deck` when `XDG_DATA_HOME` is unset (with a `-codex` suffix when `provider = "codex"`)

```
{data directory}/
  deck.db                                  Session metadata (SQLite)
  plugin/                                  deck-status plugin
  rate-limits.json                         Account rate limits reported by the sessions
  workspace/<encoded-repo>/<name>/         jj workspaces
  debug.log                                Debug log
~/.claude/projects/<project>/<uuid>.jsonl              Claude Code JSONL (read by claude-deck)
~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl           Codex JSONL (read by claude-deck when provider = "codex")
```

When each of these is created and removed is described in [data-lifecycle.md](data-lifecycle.md) (Japanese).
