# ADR-011: store を信頼できる唯一の情報源にし、SQLite に移す

## ステータス

Proposed（ADR-010 を置き換える）

## コンテキスト

セッションの状態を書き換える経路は、TUI のメモリ（`Manager`）に集まっている。CLI の `new / close` は ADR-010 のとおりソケットで TUI に依頼し、Claude Code の hook はイベントログ JSONL を TUI が読んで反映する。そのため TUI が起動していないと、CLI は使えず、hook の内容も反映されない。

store を信頼できる唯一の情報源にし、CLI と hook が直接 store に書き、TUI は読むだけにしたい。そうすると書き手が複数のプロセスになり、今の store では次の問題が起きる。

- セッション 1 件を 1 つの JSON ファイルにまとめて上書きしているので、別のプロセスの更新を消してしまう
- `os.WriteFile` は書き込みの途中を読まれうる。`LoadAll` は壊れた JSON のセッションを読み飛ばす
- ADR-008 の `killing` フラグはプロセス内でしか効かない。TUI と CLI が同じセッションを同時に close できる

ADR-010 が CLI から直接起動する案を却下した理由の 1 つは、終了監視の goroutine が起動したプロセスにしか付かないことだった。この点を調べたところ、現行の終了検知自体が動いていないと分かった。

- 終了検知は、ウィンドウの `pane-exited` hook が `wait-for -S` を鳴らし、TUI の goroutine がそれを待つ仕組みになっている（`backend_tmux.go`）
- tmux 3.6a で確かめると、ウィンドウやペインに付けた `pane-exited` hook は、ペインが自然に終了しても発火しなかった。クライアントが接続していてもいなくても同じだった。セッションに付けた hook は、終了したペインとは別のセッションのものが発火した（2026-10-09、専用ソケット `tmux -L` で確認）
- そのため `/exit` やクラッシュで終了したセッションは、TUI を起動し直して `ReconcileTmux` が走るまで Completed にならないはず。`x` は Kill の経路が自分で Completed を書くので影響を受けない

## 決定

### store を SQLite にする

- ドライバは cgo が要らない `modernc.org/sqlite` を使う。WAL モードにし、`busy_timeout` を設定する
- 更新は列単位の `UPDATE` にする。状態遷移は `UPDATE ... WHERE status = ?` で、確認と更新を 1 文で行う（二重 close はこれで防ぐ）
- セッションをまたぐ条件（名前の一意性、ClaudeSessionID が別のセッションに使われていないか）はトランザクションで確認する

### 書き手を分ける

| 書き手 | 書くもの |
|---|---|
| CLI `new` | セッション行の作成、tmux ウィンドウの作成 |
| CLI `close`（TUI の `x` も同じ処理を呼ぶ） | Completed、ウィンドウの削除 |
| ペインの後処理 `claude-deck internal session-exited <ID>` | Completed、/clear 直後に終了したときの chain の巻き戻し |
| Claude Code の hook（CLI 経由） | ステータス、SessionChain |
| TUI 起動時の `ReconcileTmux` | 上の経路から漏れた終了の補完 |

TUI は store を読んで表示する。変更は `PRAGMA data_version` を短い間隔でポーリングして検知する。ADR-010 のソケットは廃止する。

### 終了検知はウィンドウのコマンドを包んで行う

tmux の hook には頼らず、ウィンドウのコマンドを `<claude ...>; <絶対パス>/claude-deck internal session-exited <ID>` にする。後処理を実行するのは、claude と同じペインで動いているシェルになる。2026-10-09 に、本体を sleep に置き換えて確かめた結果は次のとおり。

| 終了の仕方 | 後処理 |
|---|---|
| 本体が自分で終了（`/exit`・クラッシュ相当） | 動く。終了コードも渡る |
| 本体に SIGTERM | 動く |
| 包んでいるシェルに SIGTERM、`kill-window` | 動かない。本体はペインと一緒に終了し、取り残されない |

後処理が動かないのは claude-deck が自分で終了させる経路だけなので、その経路が Completed を書く。

### hook からのステータス更新

`internal/hooks` のイベントログ JSONL はやめ、hook が CLI を呼んで store に書く。2026-10-09 に Mods の function hook で確かめた結果、次の点は classic hook だけでは扱えない。

- 承認ダイアログで拒否すると、`Stop`・`PermissionDenied`・`PostToolUseFailure` のどれも発火しない。Mods の `turn.complete` は発火する
- 承認待ちは `PermissionRequest` で判定する。auto モードでは `tool.check` が ask を返してもダイアログは出ない

## 結果

**良い点**
- TUI が起動していなくても、CLI と hook の内容が store に残る。TUI は起動時に store を読むだけで最新の状態になる
- `/exit` やクラッシュで終了したセッションが、その時点で Completed になる
- 書き込みの競合を SQLite のトランザクションに任せられる

**悪い点**
- バイナリの絶対パスをウィンドウのコマンドに埋め込む。バイナリを移動すると後処理が失敗し、次の `ReconcileTmux` まで終了が記録されない
- `pane_pid` は claude ではなく包んでいるシェルになる。シェルは claude と同時に終了するので、生存確認には使える
- store を直接読み書きする外部ツールは、JSON でなく SQLite を扱う必要がある
- 既存の JSON の store からの移行処理が要る

**却下した代替案**
- JSON ファイルのまま flock と rename で守る案。1 件ごとの読み込みと更新は守れるが、セッションをまたぐ条件にはグローバルロックが要り、トランザクションの一部を自前で作り直すことになる
- `pane-exited` hook で `run-shell` を実行する案。ウィンドウやペインに付けた hook は発火しなかった（コンテキスト参照）
- グローバル（`-g`）の `pane-exited` hook。tmux サーバーのすべてのペインに効くので、claude-deck 以外のペインでも動く。発火するかは確かめていない
- ADR-010 のソケットを残して TUI が書き込みを代行する案。TUI が起動していないと何も書けない問題が残る

## 未決事項

- hook を Mods に移すか、classic hook と併用するか。拒否後に Idle へ戻すには Mods の `turn.complete` が必要
- バックグラウンドのサブエージェントが結果を返すと、`UserPromptSubmit` と `turn.start` が発火し、人の入力と区別できない。Running 判定への影響を確かめる
