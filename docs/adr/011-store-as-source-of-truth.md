# ADR-011: store を信頼できる唯一の情報源にし、SQLite に移す

## ステータス

Accepted（ADR-010 を置き換える）

## コンテキスト

TUI・CLI・Claude Code の hook が、同じ store を読み書きする構成にしたい。エージェント（Claude Code のセッション）が CLI で deck セッションを作成・検索・終了でき、そのセッションを TUI からも同じように管理できる必要がある。TUI は store の状態と、そこから辿れる tmux・Claude Code・jj ワークスペースの情報を表示する。

ADR-010 では、セッションの状態を TUI プロセスの `Manager` だけがメモリに持ち、CLI はソケットで TUI に依頼していた。hook はイベントログ JSONL に追記し、TUI がそれを読んで反映していた。この構成では TUI が起動していないと CLI は使えず、hook の内容も反映されない。

store を書くプロセスが複数になると、当時の store では次の問題が起きる。

- セッション 1 件を 1 つの JSON ファイルにまとめて上書きしているので、別のプロセスの更新を消してしまう
- `os.WriteFile` は書き込みの途中を読まれうる。`LoadAll` は壊れた JSON のセッションを読み飛ばす
- ADR-008 の `killing` フラグはプロセス内でしか効かない。TUI と CLI が同じセッションを同時に close できる

ADR-010 が CLI から直接起動する案を却下した理由の 1 つは、終了監視の goroutine が起動したプロセスにしか付かないことだった。調べたところ、当時の終了検知自体が動いていなかった。

- 終了検知は、ウィンドウの `pane-exited` hook が `wait-for -S` を鳴らし、TUI の goroutine がそれを待つ仕組みだった
- tmux 3.6a で確かめると、ウィンドウやペインに付けた `pane-exited` hook は、ペインが自然に終了しても発火しなかった。クライアントが接続していてもいなくても同じだった。セッションに付けた hook は、終了したペインとは別のセッションのものが発火した（2026-10-09、専用ソケット `tmux -L` で確認）
- そのため `/exit` やクラッシュで終了したセッションは、TUI を起動し直して `ReconcileTmux` が走るまで Completed にならなかったはず。`x` は Kill の経路が自分で Completed を書くので影響を受けなかった

## 決定

### store を SQLite にする

- `{DataDir}/deck.db` に置く。ドライバは cgo が要らない `modernc.org/sqlite` を使い、WAL モードと `busy_timeout` を設定する
- 更新はすべて `BEGIN IMMEDIATE` のトランザクション内で行の読み込みと書き戻しをする（`store.Update` / `store.Tx`）。DEFERRED だと 2 プロセスが同時に読んだ後の書き込みが `busy_timeout` を待たずに失敗する
- 状態遷移は store の行に対する純関数にまとめ（`internal/session/transitions.go`）、どのプロセスも同じ関数をトランザクション内で適用する
- 旧形式の `sessions/*.json` は、store が空のときに 1 度だけ取り込み、ディレクトリを `sessions.migrated-<日時>` に改名する

### 書き手を分ける

| 書き手 | 書くもの |
|---|---|
| CLI `new` と TUI の `n` / `f` | セッション行の作成、tmux ウィンドウの作成 |
| CLI `close` と TUI の `x` | Completed、ワークスペースの削除、jj revision |
| TUI の `r` | Idle への復帰、ワークスペースの再作成 |
| ペインの終了コマンド `claude-deck hook exited` | Completed、/clear 直後に終了したときの chain の巻き戻し |
| deck-status プラグイン（`claude-deck hook status` / `session-start`） | ステータス、SessionChain |
| TUI 起動時の `ReconcileTmux` と 5 秒ごとの確認 | 上の経路から漏れた終了の補完 |
| TUI の終了時 | JSONL と jj から読んだ表示用の値（トークン数、プロンプト、ブックマーク） |

TUI の `Manager` が持つセッション一覧は、store を読み込んだ写しになる。TUI は `PRAGMA data_version` を 200ms ごとに確かめ、変わっていれば読み込み直す。JSONL から発見した外部セッションは store に入れず、TUI のメモリだけに持つ。ADR-010 のソケットは削除した。

行を作るのはプロセスを起動する前にする。プラグインの hook と、TUI の孤立ウィンドウ削除が、ウィンドウができた時点で行を見つけられるようにするためである。行には起動が終わるまで `launching_at` を付ける。起動中の行は、ウィンドウがまだ無くても終了扱いにせず、close もさせない。

同じセッションを 2 つのプロセスが同時に close しないよう、close の開始時に `closing_at` を書く。起動や close の途中でプロセスが落ちても行が動かせなくならないよう、`launching_at` と `closing_at` は 2 分を過ぎたら無視する。

ウィンドウが無い行を終了扱いにするときは、store を読んでから tmux のウィンドウ一覧を取り、書く直前にトランザクション内で行を読み直す。孤立ウィンドウを消すときは逆に、ウィンドウ一覧を先に取ってから store を読む。行は必ずウィンドウより先に作られるので、どちらの順序でも別のプロセスが起動中のセッションを誤って終了扱いにしたり、ウィンドウを消したりしない。再開も、行が終了状態でなければ失敗させて、2 つのプロセスが同時に再開しないようにする。

### 終了検知はウィンドウのコマンドを包んで行う

tmux の hook には頼らず、ウィンドウのコマンドを `<claude ...>; <claude-deck の絶対パス> hook exited --session <ID>` にする。2026-10-09 に、本体を sleep に置き換えて確かめた結果は次のとおり。

| 終了の仕方 | 後処理 |
|---|---|
| 本体が自分で終了（`/exit`・クラッシュ相当） | 動く。終了コードも渡る |
| 本体に SIGTERM | 動く |
| 包んでいるシェルに SIGTERM、`kill-window` | 動かない。本体はペインと一緒に終了し、取り残されない |

後処理が動かない経路のうち、`kill-window` は claude-deck の close が使うもので、close 自身が Completed を書く。tmux から直接ウィンドウを消した場合や、claude-deck のバイナリを移動した場合は、TUI の 5 秒ごとの確認がウィンドウの消えた行を Completed にする。

### hook は Mods のプラグインから CLI を呼ぶ

hook の受け口は Claude Code の Mods（function hooks）で書いた deck-status プラグイン（`deckmod/`）にする。claude-deck のバイナリに埋め込み、`{DataDir}/plugin` に展開して、起動するすべてのセッションに `--plugin-dir` で渡す。プラグインは `claude-deck hook status|session-start` を呼び、store に直接書く。

classic hook ではなく Mods を使う理由は、2026-10-09 に Mods の hook で確かめた次の点による。

- 承認ダイアログで拒否すると、classic の `Stop`・`PermissionDenied`・`PostToolUseFailure` のどれも発火しない。Mods の `turn.complete` は発火するので、拒否した後も Idle に戻せる
- 承認待ちは `PermissionRequest` で判定する。auto モードでは `tool.check` が ask を返してもダイアログは出ない
- deck の ID は環境変数 `CLAUDE_DECK_SESSION_ID` から読める。/clear のたびに SessionEnd と SessionStart を対応付ける処理が要らない

セッションには `CLAUDE_DECK_SESSION_ID` のほか、`CLAUDE_DECK_BIN`（プラグインが呼ぶバイナリ）と `CLAUDE_DECK_DATA_DIR`（store の場所）を渡す。tmux のペインは TUI でなく tmux サーバーの環境を引き継ぐので、設定ファイルの場所が TUI と違っても hook が同じ store に書くよう、data_dir を明示的に渡す。

## 結果

**良い点**
- TUI が起動していなくても、CLI と hook の内容が store に残る。TUI は起動時に store を読むだけで最新の状態になる
- エージェントが CLI で作ったセッションも、TUI の `n` で作ったものと区別なく TUI から再開・終了できる
- `/exit` やクラッシュで終了したセッションが、その時点で Completed になる
- 承認ダイアログで拒否した後も Idle に戻る
- 書き込みの競合を SQLite のトランザクションに任せられる

**悪い点**
- バイナリの絶対パスをウィンドウのコマンドに埋め込む。バイナリを移動すると後処理が失敗し、次の確認まで最大 5 秒（TUI が起動していなければ次の起動まで）終了が記録されない
- `pane_pid` は claude ではなく包んでいるシェルになる。シェルは claude と同時に終了するので、生存確認には使える
- ツール呼び出しのたびにプラグインが claude-deck を起動する（同じステータスが続く間は省く）
- 移行前に起動したセッションには deck-status プラグインが読み込まれていない。再開するまでステータスは変わらず、終了は 5 秒ごとの確認でだけ記録される

**却下した代替案**
- JSON ファイルのまま flock と rename で守る案。1 件ごとの読み込みと更新は守れるが、セッションをまたぐ条件にはグローバルロックが要り、トランザクションの一部を自前で作り直すことになる
- `pane-exited` hook で `run-shell` を実行する案。ウィンドウやペインに付けた hook は発火しなかった（コンテキスト参照）
- グローバル（`-g`）の `pane-exited` hook。tmux サーバーのすべてのペインに効くので、claude-deck 以外のペインでも動く。発火するかは確かめていない
- ADR-010 のソケットを残して TUI が書き込みを代行する案。TUI が起動していないと何も書けない問題が残る
- classic hook のシェルスクリプトから CLI を呼ぶ案。拒否した後に Idle へ戻す手段がない（Mods を選んだ理由を参照）

## 残っている課題

- バックグラウンドのサブエージェントが結果を返すと、`UserPromptSubmit` と `turn.start` が発火し、人の入力と区別できない。メインのループが結果を処理している間は実際に動いているので、Running にするのは誤りではないが、確かめてはいない
- deck-status プラグインはローカルの `--plugin-dir` でだけ読み込む。marketplace での配布は決めていない
