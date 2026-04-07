# システム概要

claude-deck は **複数の Claude Code セッションを一括管理する TUI ダッシュボード**。

ユーザーが Claude Code を複数のプロジェクト・ブランチで並行して走らせ、承認待ち・質問待ちのセッションに素早く切り替えて対処することを支援する。

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
│  セッション一覧 / detail pane / PTY 入力                  │
└────┬─────────┬──────────┬──────────┬─────────────────────┘
     │         │          │          │
     ▼         ▼          ▼          ▼
┌─────────┐ ┌──────┐ ┌────────┐ ┌────────────┐
│ Claude  │ │ jj   │ │Ghostty │ │ ファイル    │
│  Code   │ │(VCS) │ │(端末)  │ │ システム   │
│  CLI    │ │      │ │        │ │            │
└─────────┘ └──────┘ └────────┘ └────────────┘
  PTY/Hook    Workspace  外部端末    JSONL/Store
  プロセス管理  作成/削除   起動       読み書き
```

### 外部システムとの関係

| 外部システム | claude-deck との関係 |
|-------------|---------------------|
| **Claude Code CLI** | PTY プロセスとして起動・管理。Hook イベントで状態変化を通知される。JSONL ログから対話履歴とトークン使用量を読み取る |
| **jj (Jujutsu)** | セッションごとに隔離されたワークスペースを作成。ブックマーク名をセッションラベルに使用 |
| **Ghostty** | 外部ターミナルウィンドウの起動。将来的に detail pane の外部ホスティングに使用予定 |
| **ファイルシステム** | JSONL ログ監視 (fsnotify)、Hook イベントファイル監視、Store 永続化 |

## C4 Container: プロセスとデータストア

```
┌─ claude-deck プロセス ─────────────────────────────────────────────┐
│                                                                    │
│  ┌──────────┐    ┌───────────────────────────────────────┐        │
│  │   TUI    │◄───│  Session Manager                      │        │
│  │ (Bubble  │    │  ┌──────────┐  ┌──────────────────┐   │        │
│  │   Tea)   │    │  │ Session  │  │ ProcessSupervisor│   │        │
│  │          │    │  │ (N 個)   │  │ (PTY lifecycle)  │   │        │
│  │ Snapshot │    │  └──────────┘  └──────────────────┘   │        │
│  │ で読む   │    │  ┌──────────────┐  ┌──────────────┐   │        │
│  └──────────┘    │  │hookProcessor │  │ FileWatcher  │   │        │
│                  │  │(Event対応)   │  │ (JSONL監視)  │   │        │
│                  │  └──────────────┘  └──────────────┘   │        │
│                  └───────────────────────────────────────┘        │
└──────────────────────────────────────────────────────────────────┘

┌─ Claude Code プロセス (N 個) ────┐
│  PTY 接続 ←→ ProcessSupervisor   │
│  Hook イベント → hookProcessor    │
│  JSONL 書き込み → FileWatcher     │
└──────────────────────────────────┘

┌─ データストア ───────────────────┐
│  ~/.claude/projects/**/*.jsonl   │  Claude Code が書く (一次データ)
│  ~/.local/share/claude-deck/     │
│    sessions/*.json               │  claude-deck が書く (メタデータ)
│    claude-deck-events.jsonl      │  Hook イベントログ
└──────────────────────────────────┘
```

### 主要なプロセス間通信

| 経路 | 手段 | 方向 |
|------|------|------|
| claude-deck → Claude Code | PTY stdin | コマンド送信 |
| Claude Code → claude-deck | PTY stdout | 出力キャプチャ (スピナー検知含む) |
| Claude Code → claude-deck | Hook JSONL | Status 遷移、SessionChain 更新 |
| Claude Code → ファイル | JSONL 書き込み | 対話履歴・トークン記録 |
| ファイル → claude-deck | fsnotify | JSONL 変更通知、外部セッション発見 |

## データフロー

Session の状態は4つのデータソースから投影 (projection) される。

```
                    ┌───────────────────────────────────────┐
                    │            Session                    │
                    │                                       │
  PTY 出力 ────────►│ IngestPTYOutput()                     │
  (バイナリ)        │   → PTYDisplay.Write() → displayCache │
                    │   → LogLines 追記                     │
                    │   → スピナー検知 → Status=Running     │
                    │                                       │
  Hook イベント ───►│ handleHookEvent()                     │
  (JSONL)          │   → Status 遷移                       │
                    │   → SessionChain 更新 (/clear)        │
                    │                                       │
  JSONL ファイル ──►│ ApplyJSONLTokens()                    │
  (Claude ログ)    │   → TokenUsage, Prompt, StartedAt     │
                    │ ApplyFileActivity()                   │
                    │   → LastActivity                      │
                    │                                       │
  Store ──────────►│ LoadExisting()                        │
  (deck JSON)      │   → ID, Name, RepoPath, Status 復元  │
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
2. **Hook** — リアルタイム通知。Status 遷移は Hook が最も正確
3. **PTY** — フォールバック。スピナー検知による Running 検出は Hook が来ない場合の補助
4. **Store** — 起動時の復元用。JSONL/Hook で上書きされる

## 表示モデル

TUI は Session の Snapshot を通じてデータを読む。ライブ PTY 表示のみ PTYDisplay を直接参照する。

```
┌─ Session ──────────────────────────────┐
│                                        │
│  Snapshot() ──► メタデータ表示          │
│    Status, TokenUsage, Prompt, etc.    │
│                                        │
│  display *PTYDisplay ──► ライブ表示    │  HostEmbedded のみ
│    .Lines() → displayCache             │
│    .CursorPosition() → カーソル配置    │
│                                        │
│  GetStructuredLogs() ──► ログ表示      │  DisplayJSONL 時
│    JSONL 由来の構造化ログエントリ       │
│                                        │
└────────────────────────────────────────┘
         │
         ▼ DisplayChannel で分岐
┌─ TUI ─────────────────────────────────┐
│                                        │
│  DisplayPTY  → ptyViewport (全画面)    │
│  DisplayJSONL → logViewport (ログ)     │
│  DisplayNone → プレースホルダ           │
│                                        │
└────────────────────────────────────────┘
```

## セッションライフサイクル (概要)

```
  User 'n' キー
       │
       ▼
  リポジトリ選択 (wizard)
       │
       ▼
  Manager.CreateSession()
    1. NewSession()           Session 構造体作成
    2. jj workspace 作成      (オプション) 隔離環境
    3. pty.Start()            Claude Code CLI 起動
    4. InitDisplay()          PTYDisplay 作成
    5. watchProcess()         プロセス監視 goroutine
       │
       ▼ Claude Code 起動
  Hook: SessionStart         SessionChain に ID 追加
       │
       ▼ 対話中
  PTY 出力 → スピナー検知     Status: Idle ←→ Running
  Hook: Notification          Status: WaitingApproval / WaitingAnswer
  Hook: Stop                  Status: Idle
       │
       ▼ 終了
  プロセス exit               Status: Completed, managed=false
       │
       ▼ 再開可能
  User 'r' キー → ResumeSession() → --resume で Claude Code 再起動
```

詳細は `architecture.md` を参照。

## パッケージマップ

```
cmd/claude-deck/          エントリポイント・依存注入

internal/
  session/                セッションドメインモデル (← 中心)
    Session               集約ルート
    Manager               オーケストレータ
    PTYDisplay            PTY 表示インフラ
    ProcessSupervisor     プロセスライフサイクル
    Snapshot              ロックフリー投影
    hookProcessor         Hook イベントペアリング

  tui/                    Bubble Tea TUI (表示層)
    Model                 TUI 状態
    View                  レンダリング (Snapshot 経由)
    Keys                  キーバインド → Manager 操作

  pty/                    PTY プロセス管理 (インフラ)
  hooks/                  Claude Code フックイベント定義 (インフラ)
  usage/                  JSONL パース・ストリーミング (インフラ)
  store/                  JSON 永続化 (インフラ)
  config/                 TOML 設定 (インフラ)
  jj/                     Jujutsu ワークスペース (インフラ)
  ghostty/                Ghostty ランチャー (インフラ)
  claudecode/             Claude Code パス解決・trust 設定 (インフラ)
  ratelimits/             レートリミット監視 (インフラ)
  debuglog/               デバッグログ (インフラ)
```

依存の方向: `tui → session → {pty, hooks, usage, store, jj}`

session パッケージがドメインの中心。インフラパッケージはドメインに依存しない。
