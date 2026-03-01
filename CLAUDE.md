# claude-deck

Claude Code セッションを一括管理する TUI ダッシュボード。

## セットアップ

### 必須環境

- Go 1.26+
- `encoding/json/v2` を使用 → **`GOEXPERIMENT=jsonv2` が必須**

### ビルド・テスト・実行

```bash
cd claude-deck
GOEXPERIMENT=jsonv2 go build -o claude-deck ./cmd/claude-deck
GOEXPERIMENT=jsonv2 go test ./...
./claude-deck
```

## アーキテクチャ概要

```
cmd/claude-deck/main.go   エントリポイント
internal/
  session/       セッションライフサイクル管理（Manager が中心）
  tui/           Bubble Tea TUI（Model, View, Keys）
  pty/           PTY プロセス管理（claude CLI のラッパー）
  hooks/         Claude Code フックイベント連携
  usage/         JSONL パース・ストリーミング・トークン集計
  config/        TOML 設定ファイル
  store/         セッションメタデータ永続化（JSON）
  ghostty/       Ghostty ターミナルランチャー
  jj/            Jujutsu ワークスペース管理
  claudecode/    Claude Code パス解決・trust 設定
  debuglog/      デバッグログ
```

詳細なアーキテクチャは [docs/architecture.md](docs/architecture.md) を参照。

## 開発時の重要事項

### ロック順序（デッドロック防止）

Manager.mu → Session.mu の順で取得すること。逆順は ABBA デッドロックを起こす。
パターン: Manager.mu で候補リストをコピー → mu 解放 → Session.mu で個別アクセス。

### セッション ID の関係

| ID | 役割 |
|----|------|
| `Session.ID` | claude-deck 内部 ID（ランダム hex） |
| `ClaudeSessionID` | Claude Code が割り当てる UUID |
| `PreviousClaudeSessionID` | /clear 前の旧 Claude UUID（revert 用） |
| `CLAUDE_DECK_SESSION_ID` | 環境変数でフックに渡す deck ID |

`/clear` で ClaudeSessionID が変わるが、deck の Session.ID は不変。
ペアリングは `CLAUDE_DECK_SESSION_ID` 環境変数で行う。

### セッションステータス遷移

```
Idle ←→ Running ←→ WaitingApproval / WaitingAnswer
  ↓                        ↓
Completed / Error      (hook: Stop → Idle)
```

- Running: PTY 出力の Braille スピナー検知
- WaitingApproval/Answer: Hook Notification イベント
- Idle: Hook Stop イベント or スピナータイムアウト (3s)
- Completed: プロセス終了

### データソース優先度

- **JSONL** (Claude Code 一次データ): Prompt, TokenUsage, StartedAt, LastActivity
- **Store** (deck メタデータ): ID, Name, RepoPath, WorkspacePath, Status, PID
- **Runtime** (メモリのみ): LogLines, JSONLLogEntries, CurrentTool, emulator

### キーバインド

| キー | 操作 |
|------|------|
| `j/k` | カーソル移動 |
| `h/l` | ペイン切替 |
| `gg/G` | 先頭/末尾 |
| `Enter/i` | PTY 入力モード / 再開 |
| `Ctrl+D` | PTY 入力モード終了 |
| `n` | 新規セッション |
| `r` | セッション再開 |
| `dd` | セッション削除（JSONL 含む完全削除） |
| `dD` | deck メタデータのみ削除（JSONL 残す、再発見される） |
| `x` | プロセス終了 |
| `t` | Ghostty ターミナル起動 |
| `/` | フィルタ |
| `tab` | 次の要手動介入セッションへジャンプ |

### ディレクトリ構成

```
~/.config/claude-deck/config.toml     設定
~/.local/share/claude-deck/
  sessions/                           セッション JSON メタデータ
  workspace/<encoded-repo>/<name>/    jj ワークスペース
  claude-deck-events.jsonl            フックイベントログ
  debug.log                           デバッグログ
~/.claude/projects/<project>/<uuid>.jsonl   Claude Code JSONL
```

### 上限値（デフォルト値、config.toml の `[session]` で変更可）

- セッション数: 30（LRU で古いものを prune）
- LogLines: 1000 行/セッション
- スクロールバック: 2000 行
- JSONL LogEntries: 500 件
- ディスカバリ対象: 過去 14 日間
- メタデータ更新間隔: 5 秒
- スピナー Idle タイムアウト: 3 秒（固定）
- UI 更新レート: 60fps / 16ms debounce（固定）
