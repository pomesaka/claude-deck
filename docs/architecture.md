# アーキテクチャ詳細

## モジュール依存関係

```
cmd/claude-deck
  ├→ internal/tui        (TUI アプリケーション)
  │    ├→ session         (セッション管理)
  │    ├→ ghostty         (ターミナルランチャー)
  │    └→ config          (設定)
  ├→ deckmod             (deck-status プラグインの埋め込みと書き出し)
  └→ internal/session    (CLI サブコマンドと hook コマンド)

internal/session (Manager)
  ├→ usage            (JSONL パース・ストリーミング)
  ├→ store            (SQLite 永続化)
  ├→ jj               (Jujutsu ワークスペース)
  ├→ tmux             (ウィンドウの作成・削除・生存確認)
  └→ debuglog         (ログ)
```

## プロセスと store

store（`{DataDir}/deck.db`）が deck セッションの信頼できる唯一の情報源で、次のプロセスが書き手になる。

| プロセス | 書くもの |
|---|---|
| TUI | セッションの作成・再開・フォーク・close、ステータスの補完、JSONL・jj から投影した項目 |
| CLI `new` / `close` | TUI と同じ処理（`Manager.CreateSession` / `Manager.Kill`） |
| `claude-deck hook status` / `session-start` | ステータス、SessionChain（deck-status プラグインが呼ぶ） |
| `claude-deck hook exited` | Completed、`/clear` 直後に終了したときの SessionChain の巻き戻し（tmux ウィンドウのコマンドが呼ぶ） |

書き込みはすべて `BEGIN IMMEDIATE` のトランザクション内の読み書きで、別プロセスの更新を上書きしない。状態遷移の規則は `internal/session/transitions.go` の純関数（`applyHookStatus`, `applySessionStart`, `applyExited`, `beginClose`, `beginResume`）で、どのプロセスも同じ関数を使う。

`Manager.sessions` は store をメモリに投影したもので、`Manager.Reload` が store から作り直す。

- store が書く項目（Status, SessionChain, PID, ワークスペース, エラーメッセージ等）は、`Reload` のたびに store に従う
- JSONL と jj から TUI が投影する項目（Prompt, TokenUsage, BookmarkName 等）は、セッションが初めてメモリに現れるときだけ store から読む。以後はメモリの値が新しく、`PersistAll` が store に書く
- JSONL から発見した外部セッション（Unmanaged）は store に入れず、メモリだけに持つ。`ResumeSession` で再開するときに `adoptExternal` が終了済みの deck セッションとして store に入れる

`Manager.WatchStore` が `PRAGMA data_version` を 200ms ごとに見て、他プロセスがコミットしたときに `Reload` を呼ぶ。`data_version` は接続ごとの値なので、専用の接続で読む。

store の初回オープン時に、旧形式の `{DataDir}/sessions/*.json` があれば一度だけ取り込み、ディレクトリを `sessions.migrated-<timestamp>` に改名する。

## 初期化フロー

```
main() → run()
  1. debuglog.Init()
  2. config.Load() → Config (TOML)
  3. claudecode.EnsureDataDirTrusted(), RestoreStatusLine()  ← 後者は以前の版が入れた statusLine を元に戻す（ADR-013）
  4. session.OpenStore(dataDir)          ← deck.db を開く（旧 JSON の取り込み含む）
  5. buildManagerConfig()                ← deckmod.Install(dataDir/plugin)
  6. session.NewManager(ctx, store, cfg)
  7. manager.LoadExisting()              ← 重複除去・古いセッションの prune → Reload
  8. manager.ReconcileTmux()             ← store と生きている tmux ウィンドウの食い違いを補正
  9. tui.NewModel(manager, cfg) → Bubble Tea 起動
 10. Background:
     a. manager.HydrateFromJSONL()      ← JSONL からトークン等を補完
     b. manager.DiscoverExternalSessions() ← 外部セッション取り込み
     c. manager.StartFileWatcher()      ← JSONL ファイル変更監視
     d. manager.StartNotifyLoop()       ← UI 更新通知 (60fps)
     e. manager.WatchStore()            ← 他プロセスの store 書き込みを反映

main() → runCLI()                       ← 第 1 引数が new / list / close / hook のとき
  config.Load() → session.OpenStore(dataDir)
    list / hook: store だけを使う
    new / close: NewManager → Reload → CreateSession / Kill（tmux を直接操作）
    gc: session.CollectGarbage（Manager も tmux も使わない）
```

CLI と hook コマンドは TUI が起動していなくても動く。

## セッションライフサイクル

外部ファイル（workspace・store・JSONL）の作成・削除タイミングは [data-lifecycle.md](data-lifecycle.md) を参照。

### 新規作成フロー

```
User 'n' キー / claude-deck new
  → TUI: リポジトリ/サブプロジェクト選択
    Enter: ワークスペース作成+起動, Ctrl+Enter: 直接起動
  → Manager.CreateSession(ctx, repoPath, workingDir, withWorkspace)
    1. NewSession(repoPath, repoName)     // deck session 作成
    2. withWorkspace なら:
       jj.CreateWorkspaceAt(repo, name, path, extraSymlinks)  // ワークスペース作成
       extraSymlinks は config.toml [projects] で指定された .env 等の symlink リスト
       claudecode.EnsureTrusted(path)   // trust ダイアログを出さない（Claude のみ）
       サブプロジェクト対応: workingDir の相対パスをワークスペース内に対応付け
    3. startNewSession:
       a. store.Insert(row)            // launching_at（起動中の印）を付けて先に行を作る
       b. tmux ウィンドウで claude を起動
          claude --name <name> --plugin-dir {DataDir}/plugin ...; claude-deck hook exited --session <ID>
          環境変数: CLAUDE_DECK_SESSION_ID, CLAUDE_DECK_DATA_DIR, CLAUDE_DECK_BIN
       c. store.Update(PID, BookmarkName)   // launching_at を消す
    4. Reload
```

行を先に作るのは、起動直後の hook と、TUI の孤児ウィンドウ掃除が、store で行を見つけられるようにするため。

### hook による状態の反映

```
Claude Code（deck-status プラグイン）
  → claude-deck hook status running --session <ID>
    → applyHookStatus: store の Status を更新
  → claude-deck hook session-start --claude-session-id <UUID> --source <source> --session <ID>
    → applySessionStart: store の SessionChain を更新
TUI: WatchStore が data_version の変化を検知 → Reload → 一覧を再描画
```

`/clear` では source=clear の SessionStart が新しい Claude セッション ID を運ぶ。deck ID は環境変数から得るため、SessionEnd との突き合わせは要らない。`--resume` の起動で届く SessionStart(startup / resume) は、SessionChain が空のときだけ ID を追加する。イベントの詳細は [hooks.md](hooks.md)。

### プロセス終了の検知

claude を起動した tmux ウィンドウのコマンドは `<claude ...>; <claude-deck> hook exited --session <ID>` で、claude が終了すると同じペインのシェルが `hook exited` を実行する。`hook exited` は `applyExited` を store のトランザクション内で適用する。

- Status を Completed にし、FinishedAt を記録する
- `/clear` の直後にメッセージを送らず終了した場合、最新の Claude セッションには会話がなく再開できないので、SessionChain の末尾を外す。外した後の末尾を別の deck セッションが持っているときは外さない（2 つの deck セッションが同じ ID を持たないようにするため）

`hook exited` が動かない終了は次の経路で補う。

| 経路 | タイミング | 対象 |
|---|---|---|
| `Manager.Kill`（TUI の `x` / `claude-deck close`） | 実行時 | tmux ウィンドウを削除する経路。ペインごと終了するので `hook exited` は動かない。Kill 自身が `applyExited` を適用する |
| `ReconcileTmux` | TUI 起動時 | 終了済みなのにウィンドウがある行を Idle に戻す。store に行がないウィンドウを削除する。未終了なのにウィンドウがない行を Completed にする |
| `markVanishedSessions` | 5 秒の更新ごと | 未終了なのにウィンドウがない行を Completed にする。`launching_at`（起動中）と `closing_at`（close 中）が 2 分以内の行は対象外 |

### close の排他

`Manager.Kill` は最初に `beginClose` で `closing_at` を立てる。別のプロセスが 2 分以内に立てていれば `ErrClosing`、起動中（`launching_at` が 2 分以内）なら `ErrLaunching` で失敗する。TUI と CLI が同じセッションを同時に close して、同じワークスペースを削除しようとするのを防ぐ。close の途中でプロセスが落ちても、2 分後には再度 close できる。

### 再開フロー

```
User 'r' キー or Enter / Manager.ResumeSession(ctx, sessionID)
    1. backend.IsActive チェック (二重起動防止)
    2. adoptExternal: 外部セッションなら終了済みの deck セッションとして store に入れる
    3. store.Update(beginResume): 終了済みの行だけ Idle に戻す。PID=0 にする
       別プロセスが同時に再開しても、片方は「終了済みでない」で失敗する
    4. ワークスペースがなければ再作成（Kill 時に保存した revision から）
    5. tmux ウィンドウで claude --resume <csID> を起動（新規作成と同じコマンド形式）
    6. store.Update(PID)
```

## TUI アーキテクチャ

### ビューモード

| モード | 画面構成 | キー処理 |
|--------|----------|----------|
| viewDashboard | リスト (35%) + 詳細 (65%) | handleDashboardKey |
| viewSelectRepo | リポジトリ選択 (全画面) | handleRepoSelectKey |

### 右ペイン表示

| セッション状態 | 内容 |
|--------------|------|
| 未終了 (DisplayTmux) | セッションの tmux ウィンドウ |
| 終了済み・外部 (DisplayJSONL) | preview ウィンドウ（`claude-deck --preview`）の JSONL 構造化ログ |

### vim マルチキーシーケンス

```go
pendingG = true → 次の 'g' で gg 実行
```

## JSONL ストリーミングシステム

### ストリーム起動

ログを読むのは preview サブプロセスで、メインプロセスは読まない。メインプロセスは選択が変わるたびに、JSONL のパスを解決して `preview-selection` に書く（`preview.WriteSpec`）。

```
preview: WatchSpec → previewStreamer.Start(spec)
  1. 前のストリームを停止
  2. go:
     a. /clear 前の JSONL を新しい順に読む（上限 max_jsonl_entries）
     b. ReadTail(512KB)            // 即時表示（末尾から読み込み）
     c. RunFrom(tailOffset)        // 以降はリアルタイム監視
        → fsnotify で JSONL 変更検知
        → 新しい行をパース → LogEntry に変換
```

### LogEntry 種別

| 種別 | 内容 |
|------|------|
| LogEntryUser | ユーザーの入力（最初の行） |
| LogEntryText | アシスタントのテキスト出力 |
| LogEntryToolUse | ツール呼び出し（名前 + 引数概要） |
| LogEntryThinking | 思考ブロック (折りたたみ) |
| LogEntryDiff | ファイル編集の diff |

サブエージェントの JSONL も再帰的に読み込む (Depth=1)。

## 外部セッション Discovery

### 段階的読み込み

```
5秒ごとの metadataTickMsg → RefreshFromJSONL()
  1. HydrateFromJSONL()                // 既存セッションのトークン更新
  2. DiscoverExternalSessions()        // 新規外部セッション取り込み
     - usage.ListAllSessions(14日, 30件, offset)
     - known セットで除外: 追跡中の全セッションの SessionChain（過去の ID を含む）
     - newExternalSession() で StatusUnmanaged セッション作成
     - offset++ (次のページ)
```

### ファイル監視 (MultiWatcher)

```
StartFileWatcher()
  → 30秒ごとに Glob で JSONL ファイルリスト更新
  → fsnotify で書き込みイベント検知
  → 2秒のデバウンス後に LastActivity 更新
  → 新ファイル検知 → handleNewFile() で外部セッション作成
```

