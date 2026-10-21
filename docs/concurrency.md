# 並行処理パターン

## ロック階層

```
Manager.mu (外側)  →  Session.mu (内側)
```

**鉄則**: Session.mu を持ったまま Manager.mu を取得しない。逆順は ABBA デッドロックを起こす。

Manager.mu を持ったまま Session.mu を取るのは、短い読み書きだけにする（`Reload` が store の行を各 Session に写すところなど）。jj やファイルの読み取りのように時間のかかる処理は、次のパターン 1 で Manager.mu を先に解放する。

## 安全なアクセスパターン

### パターン 1: コピー→解放→個別ロック

Manager.mu でセッションリストをコピーし、mu を解放してから各セッションのフィールドにアクセスする。

```go
sessions := m.copySessionsList() // Manager.mu はこの中で取って解放する

for _, s := range sessions {
    s.mu.RLock()
    csID := s.CurrentRuntimeID()
    s.mu.RUnlock()
    // csID を使った処理（JSONL の読み取りなど）
}
```

### パターン 2: Session.mu を取る関数は Manager.mu を放してから呼ぶ

`sortTime()` のように Session.mu を中で取る関数は、Manager.mu を放してから呼ぶ。ソートの比較関数で呼ぶときも同じ。

### パターン 3: TUI は Snapshot を読む

TUI は Session のフィールドを直接読まず、`Session.Snapshot()` が Session.mu の読み取りロックの下で作るコピーを読む。描画の間はどのロックも持たない。

### パターン 4: 変更通知はまとめて渡す

Manager の変更通知は、変わったセッションの ID をためておき、16ms ごとにまとめて TUI に渡す。TUI は一覧を描き直し、選択中のセッションが含まれるとき（空ならすべてが変わった扱い）だけ preview の指定（`preview-selection`）を書き直す。

## プロセス間の排他

deck セッションの状態は複数のプロセス（TUI、CLI、hook コマンド、ウィンドウの終了コマンド）が書く。排他は SQLite に任せる。

- 書き込みはすべて `BEGIN IMMEDIATE` のトランザクション内で「読んで、変えて、書く」。同時に書くプロセスは `busy_timeout`（5 秒）の範囲で順番を待つ
- 状態遷移の規則は `transitions.go` の純関数で、トランザクション内で行に適用する。二重 close や二重 resume は、遷移関数が前提の状態（`closing_at` が空、終了済み）を確かめて失敗することで防ぐ
- `Manager.reloadMu` が `Reload` を直列化する。`WatchStore` と各操作の直後の `Reload` が同時に走っても、メモリの投影は 1 つずつ更新される
- close 中（`closing_at`）と起動中（`launching_at`）の印の規則は [architecture.md の「close の排他」](architecture.md#close-の排他) にある
- ウィンドウの一覧と store を読む順序は判定ごとに決めている。終了扱いにするときは store を先に読み、トランザクション内で行を読み直してから書く。孤立ウィンドウを消すときはウィンドウを先に読む（行は必ずウィンドウより先に作られるので、後から読んだ store に含まれる）

`PRAGMA data_version` は接続ごとの値で、自分以外の接続がコミットしたときだけ変わる。`Store` は専用の接続で読む。

## TUI から外部副作用を発行する時の順序保証

- **Bubble Tea Cmd で tmux / preview 切替を直接並列発行しない**: `select-window` や `preview-selection` 書き込みのように「最後の選択だけが勝つ」べき外部副作用は、単発の `tea.Cmd` をカーソル移動ごとに発行すると完了順が逆転する。世代番号チェックだけでは、外部 I/O が開始した後の後勝ちを止められない。
  <!-- importance: high | mentions: 1 | first-seen: 2026-05 -->

対処: 右ペイン切替のような最新状態同期は、単一 worker / queue / coalescing loop で逐次化する。worker は「最新リクエストだけ」を読み、古いリクエストは I/O 開始前に破棄する。選択なし・repo 選択モードへの遷移も generation を進めるだけでなく、queue 上の pending request を無効化する。

## goroutine の停止

TUI プロセスのバックグラウンドの goroutine（`WatchStore`、JSONL のファイル監視、変更通知のループ、レート制限の監視）は、`run` が作る ctx を受け取り、ctx がキャンセルされると止まる。ctx は SIGINT と SIGTERM でキャンセルする。

JSONL のログを読む goroutine は preview サブプロセスにある（`tui/preview.go` の `previewStreamer`）。選択が変わるたびに前のストリームをキャンセルするので、動いているのは常に 1 つ。
