# claude-deck

A TUI dashboard for managing multiple [Claude Code](https://docs.anthropic.com/en/docs/claude-code) sessions.
Integrates [jj (Jujutsu)](https://github.com/jj-vcs/jj) workspaces and [Ghostty](https://ghostty.org/) terminals to orchestrate and monitor Claude Code agents in parallel.

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
- **PTY input mode** — Interact with Claude Code directly from the dashboard (`Enter`/`i`)
- **jj workspace isolation** — Each session gets its own workspace, preventing file conflicts between agents
- **Session discovery** — Automatically finds Claude Code sessions started outside claude-deck
- **Token & cost tracking** — Per-session token usage with cost estimates
- **Ghostty integration** — Open a full terminal for any session with `t`
- **Customizable theme** — Nord, Dracula, or your own palette via `config.toml`

## Demo

<!-- <--- デモ GIF (optional): 新規セッション作成 → 複数セッションが並行実行 → Tab で承認待ちにジャンプ → Enter で入力 → 完了。15-20秒程度 ---> -->

<!-- <--- セッション詳細ペインのスクリーンショット: JSONL ログビューア (上段) と PTY 出力 (下段) の分割表示。ツール呼び出しや diff が見えている状態 ---> -->

<!-- <--- 承認待ちセッションのスクリーンショット: セッションリストで承認待ちアイコンが目立っている状態 ---> -->

## Quick Start

### Prerequisites

- Go 1.26+
- [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (`claude` command)
- [jj (Jujutsu)](https://github.com/jj-vcs/jj)
- [Ghostty](https://ghostty.org/) (optional, for `t` key terminal launch)

### Install

This project uses Go 1.26's `encoding/json/v2`, so `GOEXPERIMENT=jsonv2` is required.

```bash
GOEXPERIMENT=jsonv2 go install github.com/pomesaka/sandbox/claude-deck/cmd/claude-deck@latest
```

Or build from source:

```bash
git clone https://github.com/pomesaka/sandbox.git
cd sandbox/claude-deck
GOEXPERIMENT=jsonv2 go build -o claude-deck ./cmd/claude-deck
```

### Plugin setup

claude-deck uses a [Claude Code plugin](https://code.claude.com/docs/en/plugins) to track session status (running, waiting for approval, idle). Install it from the marketplace:

```
/plugin marketplace add pomesaka/claude-deck
/plugin install claude-deck
```

Or for local development, use `--plugin-dir`:

```bash
claude --plugin-dir /path/to/claude-deck/plugin
```

> **Migrating from legacy hooks?** If you previously used claude-deck with auto-injected hooks in `~/.claude/settings.json`, install the plugin first, then clean up:
> ```bash
> claude-deck --remove-legacy-hooks
> ```

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
| `n` | New session |
| `r` | Resume session |
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
jj = "jj"

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
```

## Known Limitations

- **jj required** — Workspace isolation relies on jj. Git-only repositories need `jj git init --colocate` first.
- **macOS / Linux only** — PTY management uses Unix-specific APIs. Windows is not supported.
- **Ghostty-specific** — The `t` key terminal launch assumes Ghostty. Other terminals can be used manually.
- **Single machine** — Sessions are local. No remote or Docker-based background execution yet.
- **Claude Code hooks** — claude-deck registers hooks in `~/.claude/settings.json` on first launch. Existing hooks are preserved but check for conflicts if you use custom hooks.

## Architecture

```
cmd/claude-deck/main.go   Entry point
internal/
  session/       Session lifecycle management (Manager)
  tui/           Bubble Tea TUI (Model, View, Keys)
  pty/           PTY process management (claude CLI wrapper)
  hooks/         Claude Code hook event integration
  usage/         JSONL parsing, streaming, token aggregation
  config/        TOML configuration
  store/         Session metadata persistence (JSON)
  ghostty/       Ghostty terminal launcher
  jj/            Jujutsu workspace management
  claudecode/    Claude Code path resolution & trust settings
  debuglog/      Debug logging
```

See [docs/architecture.md](docs/architecture.md) for details.

## Data directories

```
~/.config/claude-deck/config.toml          Configuration
~/.local/share/claude-deck/
  sessions/                                Session JSON metadata
  workspace/<encoded-repo>/<name>/         jj workspaces
  debug.log                                Debug log
~/.claude/projects/<project>/<uuid>.jsonl  Claude Code JSONL (read by claude-deck)
```

## License

[MIT](LICENSE)
