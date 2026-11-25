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
│  セッション一覧 / tmux ウィンドウ管理                      │
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
| **Agent runtime CLI** | tmux ウィンドウで起動する。Claude provider は deck-status プラグイン（`--plugin-dir` で渡す）が `claude-deck hook` を実行して状態変化を store に書く。Codex provider は TUI が JSONL の runtime activity を読んで store に書く。Codex で出るステータスは Running と Idle だけで、承認待ち・質問待ちは出ない。Codex の `/clear` の後の ID も SessionChain に足さない（最初の ID だけを、同じワークスペースで起動した行に結び付ける）。どちらも JSONL ログから対話履歴を読み取る |
| **jj (Jujutsu)** | セッションごとに隔離されたワークスペースを作成。ブックマーク名をセッションラベルに使用 |
| **Ghostty** | Ghostty の中で起動すると、右にペインを分割して tmux に attach する（[画面の構成](architecture.md#画面の構成)）。`t` で別のターミナルウィンドウを開く |
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
│  CLI サブコマンドと hook コマンド                        │
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
| CLI → store | CLI サブコマンド（一覧は [CLAUDE.md](../CLAUDE.md#cli-サブコマンド)） | セッションの作成・一覧・close など |
| store → TUI | `PRAGMA data_version` を 200ms ごとに確認して `Reload` | 他プロセスの書き込みの反映 |
| Agent runtime → ファイル | JSONL 書き込み | 対話履歴 |
| ファイル → claude-deck | fsnotify | JSONL 変更通知、外部セッション発見 |

## データフロー

Session の状態は store、JSONL、jj から組み立てる。項目ごとの書き手は [architecture.md の「プロセスと store」](architecture.md#プロセスと-store) にある。

```
                    ┌───────────────────────────────────────┐
                    │            Session (メモリ)           │
                    │                                       │
  Store ──────────►│ Reload()                              │
  (deck.db)        │   → ID, Name, Status, SessionChain,   │
  hook/CLI が書く  │     PID, ワークスペース               │
                    │                                       │
  JSONL ファイル ──►│ discovery                             │
  (Agent ログ)     │   → Prompt, StartedAt                 │
                    │ ApplyFileActivity()                   │
                    │   → LastActivity                      │
                    │ RuntimeActivity (Codex)               │
                    │   → CurrentTool（Status は store へ） │
                    └───────────────────────────────────────┘
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
  Hook: turn.complete            Status: Idle / SubagentRunning / WaitingAction
       │
       ▼ 終了
  ペインで claude-deck hook exited   Status: Completed
  (x / close はウィンドウを削除し、Kill が Completed を書く)
       │
       ▼ 再開可能
  User 'r' キー → ResumeSession() → --resume で Claude Code 再起動
```

詳細は [architecture.md](architecture.md) を参照。

## パッケージマップ

```
cmd/claude-deck/          エントリポイント・CLI サブコマンド・依存注入
deckmod/                  deck-status プラグイン（埋め込み）

internal/
  session/                セッションドメインモデル (← 中心)
    Session               集約ルート
    Manager               オーケストレータ（store の投影を持つ）
    transitions           store の行に適用する状態遷移の純関数

  tui/                    Bubble Tea TUI (表示層)。preview サブプロセス（`claude-deck --preview`）の描画も持つ
    Model                 TUI 状態
    View                  レンダリング
    Keys                  キーバインド → Manager 操作

  agentruntime/           新規・再開・フォークの起動を Claude Code と Codex それぞれのコマンドと引数に変換する
  preview/                一覧のプロセスと preview サブプロセスの間で、表示する JSONL を受け渡す

  usage/                  JSONL パース・ストリーミング (インフラ)
  store/                  SQLite 永続化 (インフラ)
  tmux/                   tmux 操作 (インフラ)
  config/                 TOML 設定 (インフラ)
  jj/                     Jujutsu ワークスペース (インフラ)
  ghostty/                Ghostty ランチャー (インフラ)
  claudecode/             Claude Code パス解決・trust 設定 (インフラ)
  ratelimits/             アカウントのレート制限の保存と監視 (インフラ)
  debuglog/               デバッグログ (インフラ)
```

依存の方向（`debuglog` は省く）:

- `cmd/claude-deck` → すべて。外部の実装（trust の登録、トランスクリプトの読み手、runtime）を選んで `session.ManagerConfig` に渡す
- `tui` → `session`、`preview`、`config`、`ratelimits`、`usage`、`ghostty`
- `preview` → `session`
- `session` → `agentruntime`、`usage`、`store`、`tmux`、`jj`、`ratelimits`、`config`

session パッケージがドメインの中心。インフラパッケージ（`usage` 以下）はドメインにも互いにも依存しない。確かめるには `GOEXPERIMENT=jsonv2 go list -f '{{.ImportPath}} {{.Imports}}' ./...` を使う。
