# claude-deck

A TUI dashboard for managing multiple [Claude Code](https://docs.anthropic.com/en/docs/claude-code) sessions.
Codex CLI support is available via `runtime.provider = "codex"`.
Integrates [jj (Jujutsu)](https://github.com/jj-vcs/jj) workspaces and [Ghostty](https://ghostty.org/) terminals to orchestrate and monitor coding agents in parallel.

<!-- <--- ダッシュボード全体のスクリーンショット: 左にセッションリスト、右に詳細ペイン。複数セッションが異なるステータス(Running/Idle/承認待ち)で並んでいる状態 ---> -->

## Why

Running multiple Claude Code agents in parallel is powerful, but managing them across many terminal windows quickly becomes chaotic:

- Switching between terminals to check which agent is waiting for approval
- Missing permission prompts buried in background terminals
- No isolation between concurrent file edits

claude-deck solves this with a single dashboard that monitors all sessions, highlights those needing attention, and isolates each agent's file changes via jj workspaces.

## Features

- **Multi-session dashboard** — Monitor all Claude Code sessions in a single TUI with real-time status updates
- **Attention alerts** — Sessions waiting for approval or answers are highlighted and reachable with `Tab`
- **JSONL structured log viewer** — Browse tool calls, diffs, and assistant responses in a readable format
- **tmux session hosting** — Interact with the selected agent in its managed tmux window
- **jj workspace isolation** — Each session gets its own workspace, preventing file conflicts between agents
- **Session discovery** — Automatically finds Claude Code sessions started outside claude-deck
- **Aliases** — Give a session a readable label with `claude-deck alias`
- **Ghostty integration** — Inside Ghostty, claude-deck splits the window and attaches the right pane to the tmux session; `t` opens a full terminal for any session
- **Workspace symlinks** — Auto-symlink `.env` and other untracked files into workspaces via per-project config
- **Customizable theme** — Set your own palette via `config.toml`

## Demo

<!-- <--- デモ GIF (optional): 新規セッション作成 → 複数セッションが並行実行 → Tab で承認待ちにジャンプ → Enter で入力 → 完了。15-20秒程度 ---> -->

<!-- <--- 右ペインのスクリーンショット: 終了済みセッションの JSONL ログビューア。ツール呼び出しや diff が見えている状態 ---> -->

<!-- <--- 承認待ちセッションのスクリーンショット: セッションリストで承認待ちアイコンが目立っている状態 ---> -->

## Quick Start

### Prerequisites

- Go 1.26+
- [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (`claude` command) or Codex CLI (`codex` command)
- [jj (Jujutsu)](https://github.com/jj-vcs/jj)
- [tmux](https://github.com/tmux/tmux)
- [Ghostty](https://ghostty.org/) (optional, for the split pane and the `t` key terminal launch)

### Install

This project uses Go 1.26's `encoding/json/v2`, so `GOEXPERIMENT=jsonv2` is required.

```bash
GOEXPERIMENT=jsonv2 go install github.com/pomesaka/claude-deck/cmd/claude-deck@latest
```

Or build from source:

```bash
git clone https://github.com/pomesaka/claude-deck.git
cd claude-deck
GOEXPERIMENT=jsonv2 go build -o claude-deck ./cmd/claude-deck
```

### Status tracking

With `runtime.provider = "claude"` (the default), claude-deck tracks session status (running, waiting for approval, idle) with the `deck-status` plugin embedded in the binary. On startup it is written to `plugin/` in the [data directory](#data-directories) and passed to every Claude Code session claude-deck launches with `--plugin-dir`. No separate install is needed.

With `runtime.provider = "codex"`, the status is read from the Codex JSONL instead.

### First run

1. Make sure your repository is initialized with jj:

   ```bash
   cd your-project
   jj git init --colocate  # if not already a jj repo
   ```

2. Launch claude-deck:

   ```bash
   claude-deck
   ```

3. Press `n` to create a new session, select your repository, and a Claude Code agent starts in an isolated jj workspace.

4. When a session shows an attention indicator, press `Tab` to jump to it, then `Enter` to interact.

Existing Claude Code sessions running outside claude-deck are automatically discovered and shown as unmanaged sessions.

## Keybindings

| Key | Action |
|-----|--------|
| `j/k` | Move cursor |
| `gg/G` | Jump to top/bottom |
| `Enter` | Focus the session's tmux window / resume a finished session |
| Click | Select the session; clicking the selected one does what `Enter` does |
| `n` | New session (Enter: with workspace, Ctrl+Enter: direct) |
| `r` | Resume session |
| `f` | Fork session |
| `x` | Kill the process and remove its workspace (JSONL and metadata are kept) |
| `p` | Pin / unpin the session (pinned sessions stay together at the bottom of the list) |
| `t` | Open Ghostty terminal |
| `/` | Filter sessions (`Esc` clears the filter) |
| `Tab` | Jump to next session needing attention |
| `R` | Redraw |
| `?` | Show help |
| `Ctrl+C` | Quit (asks first) |
| `Ctrl+Z` | Quit without asking |

`f` and `t` can be rebound with `[keybinds]` (`fork`, `open_term`). The other keys are fixed.

## Configuration

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
```

### Runtime provider

- `claude`: status and `/clear` are reported by the deck-status plugin. Transcripts are read from `~/.claude/projects`.
- `codex`: Codex has no hooks. claude-deck reads the status from the Codex JSONL (`~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`) and writes it to its store, so the status is not updated while the TUI is not running.
- With `codex`, unless `data_dir` and `[tmux] session_name` are set explicitly, the data directory gets a `-codex` suffix and the tmux session is `claude-deck-codex`. Each provider has its own `deck.db`.

### Project discovery

- With `project_markers` empty (the default), only repository roots are offered.
- With `project_markers` set, claude-deck searches each repository with `fd` and also offers the directories that contain a marker. The repository root is always offered.
- A nested repository with its own `.jj` is not a subproject of the outer one. It is offered as a repository of its own.

### Workspace symlinks

`jj workspace add` does not copy untracked files such as `.env` into the new workspace. `workspace_symlinks` lists files and directories to symlink from the repository root instead.

- Paths are relative to the repository root.
- A missing source is skipped, so the same list works for repositories without `.env`.
- An existing destination is skipped (for example, a file jj already checked out).
- Absolute paths and paths containing `..` are ignored.

### Additional directories

`add_dirs` is passed as `--add-dir` when claude-deck creates, resumes or forks a session in that repository. Relative paths are resolved from the repository root. Absolute paths are used as-is.

## Known Limitations

- **jj required** — Workspace isolation relies on jj. Git-only repositories need `jj git init --colocate` first.
- **macOS / Linux only** — Sessions run in tmux. Windows is not supported.
- **Ghostty-specific** — The split pane and the `t` key terminal launch assume Ghostty. In other terminals, run `tmux attach-session -t claude-deck` in another pane yourself (claude-deck prints the command on start).
- **Single machine** — Sessions are local. No remote or Docker-based background execution yet.

## Architecture

See the package map in [docs/01-overview.md](docs/01-overview.md).

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

When each of these is created and removed is described in [docs/data-lifecycle.md](docs/data-lifecycle.md).

## Q&A

### ダッシュボードからテキストをコピーしたい

claude-deck は alt screen + マウスモードを有効にしているため、通常のマウスドラッグではテキスト選択ができません。ターミナルのネイティブ選択を使うには **Shift キー**を併用してください。

| 操作 | macOS | Linux |
|------|-------|-------|
| テキスト選択 | `Shift + ドラッグ` | `Shift + ドラッグ` |
| 矩形（ブロック）選択 | `Shift + Opt + ドラッグ` | `Shift + Ctrl + Alt + ドラッグ` |

右ペインのテキストだけをコピーしたい場合は、**矩形選択**を使うとペイン境界のボーダー文字を含めずに選択できます。

> [!NOTE]
> キー操作はターミナルによって異なる場合があります。上記は Ghostty での操作例です。

## License

[MIT](LICENSE)
