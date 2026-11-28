# アーキテクチャ詳細

## プロセスと store

store（`{DataDir}/deck.db`）が deck セッションの信頼できる唯一の情報源で、次のプロセスが書き手になる。

| プロセス | 書くもの |
|---|---|
| TUI | セッションの作成・再開・フォーク・close、ステータスの補完、JSONL・jj から投影した項目 |
| CLI `new` / `fork` / `close` | TUI と同じ処理（`Manager.CreateSession` / `Manager.ForkSession` / `Manager.Kill`） |
| `claude-deck hook status` / `session-start` | ステータス、SessionChain（deck-status プラグインが呼ぶ） |
| `claude-deck hook exited` | Completed、`/clear` 直後に終了したときの SessionChain の巻き戻し（tmux ウィンドウのコマンドが呼ぶ） |

書き込みはすべて `BEGIN IMMEDIATE` のトランザクション内の読み書きで、別プロセスの更新を上書きしない。状態遷移の規則は `internal/session/transitions.go` の関数にまとめ、どのプロセスもトランザクション内で同じ関数を使う。

`Manager.sessions` は store をメモリに写したもので、`Manager.Reload` が store から作り直す。どの項目を store に従わせるかは項目ごとに決まっていて、複数のソースの値を優先順位で選ぶ処理は無い。

- store が書く項目（Status, SessionChain, PID, ワークスペース, エラーメッセージ等）は、`Reload` のたびに store に従う
- JSONL と jj から TUI が投影する項目（Prompt, LastActivity, BookmarkName 等）は、セッションが初めてメモリに現れるときだけ store から読む。以後はメモリの値が新しく、`PersistAll` が store に書く
- JSONL から発見した外部セッション（Origin が External）は store に入れず、メモリだけに持つ。`ResumeSession` で再開するときに `adoptExternal` が終了済みの deck セッションとして store に入れる

`Manager.WatchStore` が `PRAGMA data_version` を 200ms ごとに見て、他プロセスがコミットしたときに `Reload` を呼ぶ。`data_version` は接続ごとの値なので、専用の接続で読む。

store の初回オープン時に、旧形式の `{DataDir}/sessions/*.json` があれば一度だけ取り込み、ディレクトリを `sessions.migrated-<timestamp>` に改名する。

## 初期化フロー

第 1 引数が CLI サブコマンドの名前なら `runCLI` に入り、TUI は起動しない。CLI と hook コマンドは TUI が起動していなくても動く。

TUI の起動で順序に意味があるのは次の 2 点。

- `ReconcileTmux` は `LoadExisting` の後に呼ぶ。`LoadExisting` が重複した行と古い行を消した後の store を、生きている tmux のウィンドウと突き合わせる
- 外部セッションの発見（`DiscoverExternalSessions`）はバックグラウンドで始め、TUI を先に表示する。続きは 5 秒ごとの更新で少しずつ読む

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
       ManagerConfig.TrustWorkspaceFunc(path)   // trust ダイアログを出さない。Claude では cmd が claudecode.EnsureTrusted を渡し、Codex では nil
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

`ReconcileTmux` は、設定した tmux セッション（`[tmux] session_name`）のウィンドウを、このデータディレクトリの store だけと突き合わせる。そのため 1 つの tmux セッションを使うのは 1 つのデータディレクトリだけ、という前提で動く。`CLAUDE_DECK_DATA_DIR` や `data_dir` で別のデータディレクトリを使うときに `session_name` を変えないと、その TUI の起動が、もう一方のデータディレクトリのセッションのウィンドウを消す。provider が `codex` のときは、どちらも明示しなければ両方に `-codex` が付くので重ならない。
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
       失敗したら起動せず、行を Error にする
    5. tmux ウィンドウで claude --resume <csID> を起動（新規作成と同じコマンド形式）
    6. store.Update(PID)
```

## TUI アーキテクチャ

### 画面の構成

claude-deck の TUI は端末の幅全体にセッションの一覧を描く。`n` のリポジトリ選択も同じ領域を置き換えて出す。

一覧の右に出るのは claude-deck が描く画面ではなく、tmux セッション（既定は `claude-deck`）に attach したペインだ。起動時に tmux に `__preview__` ウィンドウを作り、Ghostty の中で起動していれば右にペインを分割して `tmux attach-session` を実行する。左の幅は `[ghostty] deck_width` で決め、フォーカスは一覧に戻す。Ghostty の外では attach のコマンドを表示するだけで、利用者が別のペインで実行する。終了時に `__preview__` ウィンドウと、自分で分割した Ghostty のペインを閉じる。

カーソルを動かすと、TUI が右のペインに出す tmux のウィンドウを切り替える。プロセスがあるセッション（終了済みでも外部でもない）はそのセッションのウィンドウを出す。それ以外は `__preview__` ウィンドウを出し、preview サブプロセス（`claude-deck --preview`）がそのセッションの JSONL のログを描く。切り替えの順序の規則は [concurrency.md](concurrency.md#tui-から外部副作用を発行する時の順序保証) にある。

### TUI の起動し直し（reload）

`claude-deck reload` は、動いている TUI に、そのバイナリで起動し直すよう頼む。TUI と CLI は次の取り決めでやり取りする（`internal/instance`）。

- TUI は動いている間、`{DataDir}/tui.lock` に flock を持ち、pid、起動したパス（リンクをたどる前）、ビルドの revision、起動した時刻を書く。CLI はロックを取れないことで TUI が動いていると知り、残ったファイルの pid を読まない
- CLI は SIGUSR1 を送り、ファイルの起動した時刻が変わる（起動し直した）か、断った理由が書かれるまで待つ
- TUI は、起動したパスのリンクを今たどった先で `version` を動かし、動いたらそのパスへ同じ引数で exec する。pid は変わらない。起動したパスは環境変数で新しいプロセスに引き継ぐので、次の reload もリンクをたどり直す
- 作成、再開、フォーク、終了の途中と、ダイアログを開いている間は断る。exec はこのプロセスの goroutine ごと消すため
- セッションは tmux のウィンドウで動き、状態は store にあるので止まらない。Ghostty の右ペインは閉じずに引き継ぎ、preview のウィンドウは新しいバイナリで動かし直す。カーソルのセッションも引き継ぐ
- 守るのは新しいバイナリの `version` が動くことまで。起動の途中（設定や store の読み込み）で落ちると TUI は消える

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

### Claude Code の JSONL について前提にしていること

Claude Code 2.1.305 が書いた `~/.claude/projects` の JSONL を読んで確かめた（2026-10-21）。

- サブエージェントの JSONL は `<project>/<session UUID>/subagents/agent-<id>.jsonl` にある。メインの JSONL には、サブエージェントの中のツール呼び出しは入らない（サブエージェントの JSONL の `tool_use` の ID が、メインの JSONL に 1 件も無いことで確かめた）。preview は、メインの JSONL の `progress` エントリ（`data.type` が `agent_progress`）の `agentId` を手がかりにこのファイルを読み、Depth=1 で差し込む（`usage.LogStreamer.inlineAgent`）。手元に残っている JSONL には `agent_progress` のエントリが 1 件も無く、`agentId` は `toolUseResult` に入っているので、いまはサブエージェントのログが差し込まれない
- 承認のイベントは JSONL に残らない（エントリの `type` を数えて、承認に当たるものが無いことで確かめた）。WaitingApproval と WaitingAnswer は deck-status プラグインの `PermissionRequest` だけから出す（[hooks.md](hooks.md)）
- 外部セッションの発見とファイル監視は `<project>/*.jsonl` を Glob で集める（`usage.claudeFormat.files`）ので、`subagents/` の下のファイルは対象にならない。どちらも念のため `subagents` を含むパスを除く（`usage.isSubagentPath`）

## 外部セッション Discovery

### 段階的読み込み

```
5秒ごとの metadataTickMsg → RefreshFromJSONL()
  1. markVanishedSessions()            // ウィンドウの消えたセッションを終了扱いにする
  2. refreshBookmarks()                // jj のブックマークを読み直す
  3. DiscoverExternalSessions()        // 新規外部セッション取り込み
     - usage.ListAllSessions(14日, 30件, offset)
     - known セットで除外: 追跡中の全セッションの SessionChain（過去の ID を含む）
     - newExternalSession() で Origin が External のセッション作成
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

