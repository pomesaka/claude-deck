# 並行処理パターン

## ロック階層

Session には2つのロックがあり、それぞれ独立したリソースを保護する:

```
Manager.mu (外側)  →  Session.mu (内側)

Session.rt.mu        独立 (JSONL ログ専用)
Session.mu           その他全フィールド
```

**鉄則**:
- Manager.mu を持ったまま Session.mu を取得しない (コピー→解放→個別ロック)
- rt.mu と Session.mu は同時に保持しない

## 安全なアクセスパターン

### パターン 1: コピー→解放→個別ロック

Manager.mu でセッションリストをコピーし、mu を解放してから各セッションのフィールドにアクセス。

```go
// ✅ 安全
m.mu.RLock()
sessions := make([]*Session, 0, len(m.sessions))
for _, s := range m.sessions {
    sessions = append(sessions, s)
}
m.mu.RUnlock()  // 先に解放

for _, s := range sessions {
    s.mu.RLock()
    csID := s.ClaudeSessionID
    s.mu.RUnlock()
    // csID を使った処理
}
```

```go
// ❌ デッドロックリスク
m.mu.RLock()
for _, s := range m.sessions {
    s.mu.RLock()          // Manager.mu 保持中に Session.mu 取得
    // ...
    s.mu.RUnlock()
}
m.mu.RUnlock()
```

### パターン 2: ソートのロック回避

sortTime() や getName() は Session.mu を取るため、Manager.mu 保持中にソートしない。

```go
m.mu.RLock()
list := make([]*Session, 0, len(m.sessions))
for _, s := range m.sessions {
    list = append(list, s)
}
m.mu.RUnlock()  // ← ソート前に解放

sort.Slice(list, func(i, j int) bool {
    return list[i].sortTime().After(list[j].sortTime())  // s.mu を内部で取得
})
```

### パターン 3: notifyChange() のデバウンス

notifyChange は変更されたセッション ID を pendingChanges に蓄積し、バッファ 1 のチャネルで通知をデバウンスする。

```go
func (m *Manager) notifyChange(sessionIDs ...DeckSessionID) {
    if len(sessionIDs) > 0 {
        m.pendingMu.Lock()
        for _, id := range sessionIDs {
            m.pendingChanges[id] = true
        }
        m.pendingMu.Unlock()
    }
    select {
    case m.notifyCh <- struct{}{}:
    default: // already pending; coalesce
    }
}
```

StartNotifyLoop が 16ms (≈60fps) 間隔でドレインし、onChange コールバックに変更セット全体を渡す。TUI 側は ChangedIDs に選択中セッションが含まれる場合のみ viewport を更新する。

### パターン 4: setStatusLocked

既にロックを保持している場合に使う内部ヘルパー。

```go
sess.mu.Lock()
sess.setStatusLocked(StatusIdle)  // ロック取得済み前提
sess.FinishedAt = nil
sess.mu.Unlock()
```

## プロセス間の排他

deck セッションの状態は複数のプロセス（TUI、CLI、hook コマンド、ウィンドウの終了コマンド）が書く。排他は SQLite に任せる。

- 書き込みはすべて `BEGIN IMMEDIATE` のトランザクション内で「読んで、変えて、書く」。同時に書くプロセスは `busy_timeout`（5 秒）の範囲で順番を待つ
- 状態遷移の規則は `transitions.go` の純関数で、トランザクション内で行に適用する。二重 close や二重 resume は、遷移関数が前提の状態（`closing_at` が空、終了済み）を確かめて失敗することで防ぐ
- `closing_at` は close 中のセッションの印で、2 分でタイムアウトする。close の途中でプロセスが落ちても、後から close できる
- `Manager.reloadMu` が `Reload` を直列化する。`WatchStore` と各操作の直後の `Reload` が同時に走っても、メモリの投影は 1 つずつ更新される
- `launching_at` は起動中のセッションの印で、2 分でタイムアウトする。起動中の行は、ウィンドウがまだ無くても終了扱いにせず、close もできない
- ウィンドウの一覧と store を読む順序は判定ごとに決めている。終了扱いにするときは store を先に読み、トランザクション内で行を読み直してから書く。孤立ウィンドウを消すときはウィンドウを先に読む（行は必ずウィンドウより先に作られるので、後から読んだ store に含まれる）

`PRAGMA data_version` は接続ごとの値で、自分以外の接続がコミットしたときだけ変わる。`Store` は専用の接続で読む。

## Background Goroutine 一覧

| goroutine | 起動元 | 終了条件 | 役割 |
|-----------|--------|----------|------|
| StartNotifyLoop | main | ctx.Done() | dirty flag → onChange (60fps) |
| WatchStore | main | ctx.Done() | `data_version` を 200ms ごとに見て、変化したら `Reload` |
| MultiWatcher.Run | main | ctx.Done() | JSONL ファイル変更監視 |
| StreamSession | updateSelected | cancel() | JSONL リアルタイム読み込み |
| HydrateFromJSONL | main (init) | 完了 | 起動時トークン補完 |

## Context キャンセレーション

```
main の ctx (signal: SIGINT/SIGTERM)
  ├→ Manager.ctx (全 goroutine の親)
  │    ├→ WatchStore goroutine
  │    ├→ MultiWatcher.Run goroutine
  │    └→ NotifyLoop goroutine
  │
  └→ 個別セッションの ctx
       └→ StreamSession (activeStreamCancel)
```

activeStreamCancel は1つだけアクティブ（前のストリームはキャンセルされる）。
