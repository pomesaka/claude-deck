# claude-deck

A TUI dashboard for managing multiple [Claude Code](https://docs.anthropic.com/en/docs/claude-code) sessions.
Codex CLI support is available via `runtime.provider = "codex"`.
Integrates [jj (Jujutsu)](https://github.com/jj-vcs/jj) workspaces and [Ghostty](https://ghostty.org/) terminals to orchestrate and monitor coding agents in parallel.

<!-- <--- ダッシュボード全体のスクリーンショット: 左にセッションリスト、右に詳細ペイン。複数セッションが異なるステータス(Running/Idle/承認待ち)で並んでいる状態 ---> -->

## Why

Running multiple Claude Code agents in parallel is powerful, but managing them across many terminal windows quickly becomes chaotic:

- Switching between terminals to check which agent is waiting for approval
- Losing track of token spend across sessions
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
- **Token & cost tracking** — Per-session token usage with cost estimates
- **Ghostty integration** — Open a full terminal for any session with `t`
- **Workspace symlinks** — Auto-symlink `.env` and other untracked files into workspaces via per-project config
- **Customizable theme** — Nord, Dracula, or your own palette via `config.toml`

## Demo

<!-- <--- デモ GIF (optional): 新規セッション作成 → 複数セッションが並行実行 → Tab で承認待ちにジャンプ → Enter で入力 → 完了。15-20秒程度 ---> -->

<!-- <--- セッション詳細ペインのスクリーンショット: JSONL ログビューア (上段) と PTY 出力 (下段) の分割表示。ツール呼び出しや diff が見えている状態 ---> -->

<!-- <--- 承認待ちセッションのスクリーンショット: セッションリストで承認待ちアイコンが目立っている状態 ---> -->

## Quick Start

### Prerequisites

- Go 1.26+
- [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (`claude` command) or Codex CLI (`codex` command)
- [jj (Jujutsu)](https://github.com/jj-vcs/jj)
- [Ghostty](https://ghostty.org/) (optional, for `t` key terminal launch)

### Install

This project uses Go 1.26's `encoding/json/v2`, so `GOEXPERIMENT=jsonv2` is required.

```bash
GOEXPERIMENT=jsonv2 go install github.com/pomesaka/claude-deck/cmd/claude-deck@latest
```

Or build from source:

```bash
git clone https://github.com/pomesaka/sandbox.git
cd sandbox/claude-deck
GOEXPERIMENT=jsonv2 go build -o claude-deck ./cmd/claude-deck
```

### Status tracking

With `runtime.provider = "claude"` (the default), claude-deck tracks session status (running, waiting for approval, idle) with the `deck-status` plugin embedded in the binary. On startup it is written to `~/.local/share/claude-deck/plugin/` and passed to every Claude Code session claude-deck launches with `--plugin-dir`. No separate install is needed.

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
| `h/l` | Switch pane focus |
| `gg/G` | Jump to top/bottom |
| `Enter/i` | PTY input mode / resume session |
| `Ctrl+D` | Exit PTY input mode |
| `n` | New session (Enter: with workspace, Ctrl+Enter: direct) |
| `r` | Resume session |
| `f` | Fork session |
| `dd` | Delete session (including JSONL) |
| `dD` | Remove deck metadata only (JSONL preserved) |
| `x` | Kill process |
| `t` | Open Ghostty terminal |
| `/` | Filter sessions |
| `Tab` | Jump to next session needing attention |
| `?` | Show help |
| `Ctrl+C` | Quit |

## Configuration

Config file: `~/.config/claude-deck/config.toml`

All sections are optional. Unspecified values use built-in defaults.

```toml
[defaults]
permission_mode = "default"

[ghostty]
command = "ghostty"

[theme]
primary = "#7C3AED"
secondary = "#06B6D4"
success = "#10B981"
warning = "#F59E0B"
danger = "#EF4444"
bg_selected = "#313244"
border = "#45475A"
border_focus = "#7C3AED"
text = "#CDD6F4"
text_dim = "#6C7086"
status_idle = "#808898"
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
max_sessions = 30
max_log_lines = 1000
max_scrollback = 2000
max_jsonl_entries = 500
discovery_days = 14
refresh_interval = "5s"

[pricing]
input_per_mtok = 15.0
output_per_mtok = 75.0
cache_write_per_mtok = 18.75
cache_read_per_mtok = 1.50

# Per-project workspace symlinks
# jj workspace にはリポジトリの untracked ファイル (.env 等) がコピーされない。
# プロジェクトごとに symlink したいファイルを指定できる。
[projects."/Users/you/your-repo"]
workspace_symlinks = [".env", ".env.local", "secrets/"]
```

## Known Limitations

- **jj required** — Workspace isolation relies on jj. Git-only repositories need `jj git init --colocate` first.
- **macOS / Linux only** — PTY management uses Unix-specific APIs. Windows is not supported.
- **Ghostty-specific** — The `t` key terminal launch assumes Ghostty. Other terminals can be used manually.
- **Single machine** — Sessions are local. No remote or Docker-based background execution yet.

## Architecture

```
cmd/claude-deck/main.go   Entry point
internal/
  session/       Session lifecycle management (Manager)
  tui/           Bubble Tea TUI (Model, View, Keys)
  tmux/          tmux window management
  usage/         JSONL parsing, streaming, token aggregation
  config/        TOML configuration
  store/         Session metadata persistence (SQLite)
  ghostty/       Ghostty terminal launcher
  jj/            Jujutsu workspace management
  claudecode/    Claude Code path resolution & trust settings
  debuglog/      Debug logging
deckmod/         deck-status Claude Code plugin (embedded)
```

See [docs/architecture.md](docs/architecture.md) for details.

## Data directories

```
~/.config/claude-deck/config.toml          Configuration
~/.local/share/claude-deck/
  deck.db                                  Session metadata (SQLite)
  plugin/                                  deck-status plugin
  workspace/<encoded-repo>/<name>/         jj workspaces
  debug.log                                Debug log
~/.claude/projects/<project>/<uuid>.jsonl              Claude Code JSONL (read by claude-deck)
~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl           Codex JSONL (read by claude-deck when provider = "codex")
```

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
