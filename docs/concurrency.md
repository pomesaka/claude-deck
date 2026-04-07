# 並行処理パターン

## ロック階層

Session には3つのロックがあり、それぞれ独立したリソースを保護する:

```
Manager.mu (外側)  →  Session.mu (内側)

PTYDisplay.emuMu     独立 (エミュレータ専用)
Session.rt.mu        独立 (PTY ログ・JSONL ログ専用)
Session.mu           その他全フィールド
```

**鉄則**:
- Manager.mu を持ったまま Session.mu を取得しない (コピー→解放→個別ロック)
- PTYDisplay.emuMu は Session.mu/rt.mu と独立。ただし `onTitle` コールバックが Session.mu を取得するため、Session.mu を保持したまま `display.Write()` を呼ばない
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

## Background Goroutine 一覧

| goroutine | 起動元 | 終了条件 | 役割 |
|-----------|--------|----------|------|
| watchProcess | CreateSession / ResumeSession | proc.Done() | プロセス終了監視 |
| readLoop (pty) | pty.Start | ptmx EOF or ctx cancel | PTY 出力読み取り |
| StartNotifyLoop | main | ctx.Done() | dirty flag → onChange (60fps) |
| StartSpinnerIdleLoop | main | ctx.Done() | Running → Idle 自動遷移 |
| StartEventWatcher | main | ctx.Done() | フックイベント監視 |
| MultiWatcher.Run | main | ctx.Done() | JSONL ファイル変更監視 |
| StreamSession | updateSelected | cancel() | JSONL リアルタイム読み込み |
| HydrateFromJSONL | main (init) | 完了 | 起動時トークン補完 |

## PTY の concurrent access 保護

```go
type Process struct {
    ptmxMu     sync.Mutex  // ptmx への concurrent access をガード
    ptmxClosed bool
}

func (p *Process) Write(data []byte) (int, error) {
    p.ptmxMu.Lock()
    defer p.ptmxMu.Unlock()
    if p.ptmxClosed {
        return 0, fmt.Errorf("pty closed")
    }
    return p.ptmx.Write(data)
}

func (p *Process) closePty() {
    p.ptmxMu.Lock()
    defer p.ptmxMu.Unlock()
    if p.ptmxClosed {
        return
    }
    p.ptmxClosed = true
    _ = p.ptmx.Close()
}
```

Write/Resize/closePty が同じ mutex を共有。
プロセス終了後の Write エラーは呼び出し元で無視される。

## Context キャンセレーション

```
main の ctx (signal: SIGINT/SIGTERM)
  ├→ Manager.ctx (全 goroutine の親)
  │    ├→ WatchEvents goroutine
  │    ├→ MultiWatcher.Run goroutine
  │    ├→ NotifyLoop goroutine
  │    └→ SpinnerIdleLoop goroutine
  │
  └→ 個別セッションの ctx
       ├→ pty.Start (readLoop)
       └→ StreamSession (activeStreamCancel)
```

activeStreamCancel は1つだけアクティブ（前のストリームはキャンセルされる）。
