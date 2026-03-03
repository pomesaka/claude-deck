# アーキテクチャ詳細

## モジュール依存関係

```
cmd/claude-deck
  └→ internal/tui        (TUI アプリケーション)
       ├→ session         (セッション管理)
       ├→ ghostty         (ターミナルランチャー)
       └→ config          (設定)

internal/session (Manager)
  ├→ pty              (PTY プロセス管理)
  ├→ hooks            (Claude Code フックイベント)
  ├→ usage            (JSONL パース・ストリーミング)
  ├→ store            (JSON 永続化)
  ├→ jj               (Jujutsu ワークスペース)
  └→ debuglog         (ログ)
```

## 初期化フロー

```
main() → run()
  1. debuglog.Init()
  2. config.Load() → Config (TOML)
  3. store.New(dataDir + "/sessions")
  4. session.NewManager(ctx, store, cfg)
  5. manager.LoadExisting()              ← ストアからセッション復元
  6. hooks.EnsureHooks(dataDir)          ← Claude Code 設定にフック登録
  7. claudecode.EnsureTrust()            ← Claude Code trust 設定
  8. tui.NewModel(manager, cfg) → Bubble Tea 起動
  9. Background:
     a. manager.HydrateFromJSONL()      ← JSONL からトークン等を補完
     b. manager.DiscoverExternalSessions() ← 外部セッション取り込み
     c. manager.StartEventWatcher()     ← フックイベント監視
     d. manager.StartFileWatcher()      ← JSONL ファイル変更監視
     e. manager.StartNotifyLoop()       ← UI 更新通知 (60fps)
     f. manager.StartSpinnerIdleLoop()  ← スピナータイムアウト検知
```

## セッションライフサイクル

### 新規作成フロー

```
User 'n' キー
  → handleRepoSelectKey: リポジトリ/サブプロジェクト選択
    Enter: ワークスペース作成+起動, Ctrl+Enter: 直接起動
  → Manager.CreateSession(ctx, repoPath, workingDir, withWorkspace, cols, rows)
    1. NewSession(repoPath, repoName)     // deck session 作成
    2. withWorkspace なら:
       jj.CreateWorkspaceAt(repo, name, path, extraSymlinks)  // ワークスペース作成
       extraSymlinks は config.toml [projects] で指定された .env 等の symlink リスト
       サブプロジェクト対応: workingDir の相対パスをワークスペース内に対応付け
    3. pty.Start(ctx, opts, handleOutput)  // claude --agent <name> 起動
       opts.Env = ["CLAUDE_DECK_SESSION_ID=<sessID>"]
    4. sessions[sessID] = sess, processes[sessID] = proc
    5. persist(sess)
    6. go watchProcess(sess, proc)         // プロセス監視開始
```

### フックイベントによる ID 紐付け

```
Claude Code 起動
  → SessionStart hook {session_id: UUID, source: "startup"}
    → handleHookEvent: sessions[ClaudeDeckSessionID].ClaudeSessionID = UUID

Claude Code --resume 起動
  → SessionStart {session_id: transient, source: "startup"}
    → ClaudeSessionID = transient (一時的)
  → SessionStart {session_id: original, source: "resume"}
    → ClaudeSessionID = original (上書き、これが正しい ID)
```

### /clear 時のペアリング

```
ユーザーが /clear 実行
  1. SessionEnd {session_id: OLD, reason: "clear", CLAUDE_DECK_SESSION_ID: DECK_ID}
     → pendingEndEvents[DECK_ID] = &event
  2. SessionStart {session_id: NEW, source: "clear", CLAUDE_DECK_SESSION_ID: DECK_ID}
     → pendEnd = pendingEndEvents[DECK_ID]
     → sess = sessions[DECK_ID]
     → sess.PreviousClaudeSessionID = OLD
     → sess.ClaudeSessionID = NEW
     → oldSessionIDs[OLD] = true  (discovery で再インポート防止)
     → JSONL ストリーム再起動
```

### プロセス終了時の処理 (watchProcess)

```
<-proc.Done()
  1. sess.managed = false
  2. Status → Completed (if not already error/completed)
  3. /clear 後に新 ID の JSONL が空の場合:
     a. isClaudeIDClaimed(prevCSID) → true: revert しない (重複防止)
     b. false: ClaudeSessionID を PreviousClaudeSessionID に戻す
  4. persist(sess)
```

### 再開フロー

```
User 'r' キー or Enter
  → Manager.ResumeSession(ctx, sessionID, cols, rows)
    1. HasActiveProcess チェック (二重起動防止)
    2. pty.Start(ctx, {ResumeSessionID: csID, Env: [DECK_SESSION_ID]})
    3. sess.Status = Idle, FinishedAt = nil
    4. emulator リセット、LogLines クリア
    5. processes[sessID] = proc
    6. go watchProcess(sess, proc)
```

## TUI アーキテクチャ

### ビューモード

| モード | 画面構成 | キー処理 |
|--------|----------|----------|
| viewDashboard | リスト (35%) + 詳細 (65%) | handleDashboardKey |
| viewSelectRepo | リポジトリ選択 (全画面) | handleRepoSelectKey |

### PTY 入力モード

`ptyInputActive = true` の間:
- 全キーが PTY stdin に転送される（Ctrl+C 含む）
- `Ctrl+D` のみ入力モード終了
- `keyToBytes()` で Bubble Tea のキーイベントを PTY バイト列に変換
- マルチバイト UTF-8 文字も正しくハンドリング

### 詳細ペイン表示

| セッション状態 | 上段 | 下段 |
|--------------|------|------|
| PTY あり (managed) | JSONL 構造化ログ (logViewport) | PTY 生出力 (ptyViewport) |
| PTY なし (completed/external) | JSONL 構造化ログ (logViewport) | — |

### vim マルチキーシーケンス

```go
pendingG = true → 次の 'g' で gg 実行
pendingD = true → 次の 'd' で dd (軽量削除)、'D' で dD (完全削除)
```

## JSONL ストリーミングシステム

### ストリーム起動

```
updateSelected() → StreamSession(sessionID)
  1. stopActiveStream(prev)        // 前のストリームを停止
  2. activeStreamID = sessionID
  3. go:
     a. ReadTail(512KB)            // 即時表示（末尾から読み込み）
     b. RunFrom(tailOffset)        // 以降はリアルタイム監視
        → fsnotify で JSONL 変更検知
        → 新しい行をパース → LogEntry に変換
        → sess.JSONLLogEntries に追加 (cap 500)
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
     - known セットで除外: ClaudeSessionID, PreviousClaudeSessionID, oldSessionIDs
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

## VT100 エミュレータ

- charmbracelet/x/vt ベース（_vt_local/ にローカルコピー）
- PTY 出力を解釈して画面状態を保持
- OSC 0/2 シーケンスでターミナルタイトル抽出
- ScrollUp コールバックでスクロールバック行を蓄積
- GetPTYDisplayLines() で styled テキストを取得（スクロールバック + 画面）
