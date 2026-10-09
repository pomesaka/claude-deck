# システム概要

claude-deck は **複数の coding agent セッションを一括管理する TUI ダッシュボード**。

ユーザーが Claude Code や Codex CLI を複数のプロジェクト・ブランチで並行して走らせ、承認待ち・質問待ちのセッションに素早く切り替えて対処することを支援する。

## C4 Context: システムと外部アクター

```
                      ┌─────────────┐
                      │    User     │
                      └──────┬──────┘
                             │ キーボード操作
                             ▼
┌──────────────────────────────────────────────────────────┐
│                     claude-deck                          │
│                  (TUI ダッシュボード)                      │
│                                                          │
│  セッション一覧 / detail pane / tmux window 管理           │
└────┬─────────┬──────────┬──────────┬─────────────────────┘
     │         │          │          │
     ▼         ▼          ▼          ▼
┌─────────┐ ┌──────┐ ┌────────┐ ┌────────────┐
│ Agent   │ │ jj   │ │Ghostty │ │ ファイル    │
│ Runtime │ │(VCS) │ │(端末)  │ │ システム   │
│  CLI    │ │      │ │        │ │            │
└─────────┘ └──────┘ └────────┘ └────────────┘
  tmux/Hook   Workspace  外部端末    JSONL/Store
  プロセス管理  作成/削除   起動       読み書き
```

### 外部システムとの関係

| 外部システム | claude-deck との関係 |
|-------------|---------------------|
| **Agent runtime CLI** | tmux ウィンドウで起動する。Claude provider は deck-status プラグイン（`--plugin-dir` で渡す）が `claude-deck hook` を実行して状態変化を store に書く。Codex provider は TUI が JSONL の runtime activity を読んで store に書く。どちらも JSONL ログから対話履歴とトークン使用量を読み取る |
| **jj (Jujutsu)** | セッションごとに隔離されたワークスペースを作成。ブックマーク名をセッションラベルに使用 |
| **Ghostty** | 外部ターミナルウィンドウの起動。将来的に detail pane の外部ホスティングに使用予定 |
| **ファイルシステム** | JSONL ログ監視 (fsnotify)、Store（SQLite `deck.db`）の読み書きと変更監視 |

## C4 Container: プロセスとデータストア

```
┌─ claude-deck TUI プロセス ───────────────────────────────┐
│  ┌──────────┐    ┌───────────────────────────────┐      │
│  │   TUI    │◄───│  Session Manager              │      │
│  │ (Bubble  │    │  sessions = store の投影      │      │
│  │   Tea)   │    │  ┌─────────────┐ ┌──────────┐ │      │
│  │ Snapshot │    │  │ WatchStore  │ │FileWatcher│ │      │
│  │ で読む   │    │  │(data_version)│ │(JSONL監視)│ │      │
│  └──────────┘    │  └─────────────┘ └──────────┘ │      │
│                  └───────────────────────────────┘      │
└─────────────────────────────────────────────────────────┘

┌─ claude-deck CLI / hook プロセス（短命） ───────────────┐
│  new / list / close / hook status|session-start|exited  │
│  store を直接読み書きする。new / close は tmux も操作    │
└─────────────────────────────────────────────────────────┘

┌─ Agent runtime プロセス (N 個、tmux ウィンドウ内) ──────┐
│  Claude: deck-status プラグイン → claude-deck hook ...   │
│  終了後に同じペインで claude-deck hook exited            │
│  JSONL 書き込み → FileWatcher                            │
└─────────────────────────────────────────────────────────┘

┌─ データストア ───────────────────┐
│  ~/.claude/projects/**/*.jsonl   │  Claude Code が書く (一次データ)
│  ~/.codex/sessions/**/*.jsonl    │  Codex が書く (一次データ)
│  ~/.local/share/claude-deck/     │
│    deck.db                       │  SQLite。deck セッションの信頼できる唯一の情報源
│    plugin/                       │  deck-status プラグイン
└──────────────────────────────────┘
```

### 主要なプロセス間通信

| 経路 | 手段 | 方向 |
|------|------|------|
| claude-deck → Agent runtime | tmux ウィンドウの作成・削除 | 起動・resume・fork・終了 |
| Claude Code → store | deck-status プラグインが `claude-deck hook` を実行 | Status 遷移、SessionChain 更新 |
| Codex の JSONL → store | TUI が runtime activity と JSONL の発見から書く | Status 遷移、SessionChain の最初の ID |
| ペインのシェル → store | runtime 終了後に `claude-deck hook exited` を実行 | Completed の記録 |
| CLI → store | `claude-deck new / list / close` | セッションの作成・一覧・close |
| store → TUI | `PRAGMA data_version` を 200ms ごとに確認して `Reload` | 他プロセスの書き込みの反映 |
| Agent runtime → ファイル | JSONL 書き込み | 対話履歴・トークン記録 |
| ファイル → claude-deck | fsnotify | JSONL 変更通知、外部セッション発見 |

## データフロー

Session の状態は複数のデータソースから投影 (projection) される。

```
                    ┌───────────────────────────────────────┐
                    │            Session (メモリ)           │
                    │                                       │
  Store ──────────►│ Reload()                              │
  (deck.db)        │   → ID, Name, Status, SessionChain,   │
  hook/CLI が書く  │     PID, ワークスペース               │
                    │                                       │
  JSONL ファイル ──►│ ApplyJSONLTokens()                    │
  (Agent ログ)     │   → TokenUsage, Prompt, StartedAt     │
                    │ ApplyFileActivity()                   │
                    │   → LastActivity                      │
                    │ RuntimeActivity (Codex)               │
                    │   → CurrentTool（Status は store へ） │
                    │                                       │
                    │           ┌──────────┐                │
                    │           │ Snapshot  │───────► TUI   │
                    │           │ (ロック   │  レンダリング  │
                    │           │  フリー)  │                │
                    └───────────┴──────────┘────────────────┘
```

### データソースの優先度

同じフィールドに複数のソースが書き込む場合の優先順位:

1. **JSONL** (最優先) — Claude Code の一次記録。TokenUsage, Prompt, StartedAt
2. **Hook** — リアルタイム通知。Status 遷移は Hook が最も正確。`claude-deck hook` が store に書き、TUI は Store 経由で受け取る
3. **Store** — deck セッションの状態の信頼できる唯一の情報源。TUI 起動時の復元にも使う

## 表示モデル

TUI は Session の Snapshot を通じてデータを読む。

```
┌─ Session ──────────────────────────────┐
│  Snapshot() ──► メタデータ表示          │
│    Status, TokenUsage, Prompt, etc.    │
│                                        │
└────────────────────────────────────────┘
         │
         ▼ DisplayChannel で分岐
┌─ 右ペイン ────────────────────────────┐
│  DisplayTmux  → セッションの tmux ウィンドウ                     │
│  DisplayJSONL → preview ウィンドウが JSONL を読んでログを表示    │
└────────────────────────────────────────┘
```

## セッションライフサイクル (概要)

```
  User 'n' キー / claude-deck new
       │
       ▼
  Manager.CreateSession()
    1. NewSession()           Session 構造体作成
    2. jj workspace 作成      (オプション) 隔離環境
    3. store.Insert()         PID=0 で行を作成
    4. tmux ウィンドウで claude を起動（--plugin-dir 付き）
    5. store.Update(PID)
       │
       ▼ Claude Code 起動
  Hook: SessionStart         SessionChain に ID 追加
       │
       ▼ 対話中
  Hook: turn.start / tool.call   Status: Running
  Hook: PermissionRequest        Status: WaitingApproval / WaitingAnswer
  Hook: turn.complete            Status: Idle
       │
       ▼ 終了
  ペインで claude-deck hook exited   Status: Completed
  (x / close はウィンドウを削除し、Kill が Completed を書く)
       │
       ▼ 再開可能
  User 'r' キー → ResumeSession() → --resume で Claude Code 再起動
```

詳細は `architecture.md` を参照。

## パッケージマップ

```
cmd/claude-deck/          エントリポイント・CLI サブコマンド・依存注入
deckmod/                  deck-status プラグイン（埋め込み）

internal/
  session/                セッションドメインモデル (← 中心)
    Session               集約ルート
    Manager               オーケストレータ（store の投影を持つ）
    transitions           store の行に適用する状態遷移の純関数
    Snapshot              ロックフリー投影

  tui/                    Bubble Tea TUI (表示層)
    Model                 TUI 状態
    View                  レンダリング (Snapshot 経由)
    Keys                  キーバインド → Manager 操作

  usage/                  JSONL パース・ストリーミング (インフラ)
  store/                  SQLite 永続化 (インフラ)
  tmux/                   tmux 操作 (インフラ)
  config/                 TOML 設定 (インフラ)
  jj/                     Jujutsu ワークスペース (インフラ)
  ghostty/                Ghostty ランチャー (インフラ)
  claudecode/             Claude Code パス解決・trust 設定 (インフラ)
  ratelimits/             レートリミット監視 (インフラ)
  debuglog/               デバッグログ (インフラ)
```

依存の方向: `tui → session → {usage, store, tmux, jj}`

session パッケージがドメインの中心。インフラパッケージはドメインに依存しない。
