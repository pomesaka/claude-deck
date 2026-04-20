# Split Mode: 右ペイン制御 設計ドキュメント

> **注意**: 本文書は split mode 導入時の設計分析メモです。
> 実装の最終仕様は ADR-004 および `internal/session/session.go:displayChannel()` を参照してください。

## 概要

Split mode（Ghostty 分割 + tmux backend）では、右ペインは「選択中セッションの状態」を常に反映すべきである。

```
左ペイン (claude-deck TUI)        右ペイン (tmux attach -t claude-deck)
  ● session-A  running       →   session-A の tmux window
  > session-B  ← cursor      →   (session-B の状態に応じて切替)
  ○ session-C  completed
```

---

## 状態モデル

### DisplayChannel（セッション状態から自動導出）

```go
// 現在の実装 (session.go:displayChannel)
func displayChannel() DisplayChannel {
    p := s.process.Load()
    if p == nil {
        return DisplayJSONL    // プロセスなし → ログ表示
    }
    if p.IsEmbedded() {
        return DisplayPTY      // PTY mode（split mode 外）
    }
    return DisplayNone         // tmux mode で稼働中
}
```

| RunningProcess | IsEmbedded | DisplayChannel |
|----------------|------------|----------------|
| nil | — | **DisplayJSONL** — 停止、ログを表示 |
| non-nil | false | **DisplayNone** — tmux window で稼働中 |
| non-nil | true | DisplayPTY — PTY mode（split mode 外） |

### 右ペイン表示ルール

| 選択セッションの状態 | 右ペイン |
|---|---|
| DisplayNone (起動中) | そのセッションの tmux window (`tmux select-window <sessionID>`) |
| DisplayJSONL/DisplayPTY (停止中) | `__preview__` window + IPC で選択セッション ID を通知 |

### Ghostty ペインフォーカス ルール

右ペイン「内容の切替」と「フォーカスの移動」は **別の操作** である。

| ユーザーアクション | 内容切替 | フォーカス移動 |
|---|---|---|
| `j/k` カーソル移動 | ✓ | ✗（ユーザーはリストを閲覧中） |
| `Enter` 起動中セッション | ✓ | ✓ |
| `Enter` / `r` 停止中→再開 | ✓ | ✓ |
| `n` 新規作成 | ✓ | ✓ |
| `f` フォーク | ✓ | ✓ |
| `x` kill | ✓ | ✗ |

---

## 実装設計

### 単一責任の原則

現在の問題: 右ペイン制御ロジックが以下に散在している。

- `updateSelected()` — display 差分検知
- `splitModeFocusCmd()` — 新規/フォーク用
- `sessionResumedMsg` ハンドラ — resume 用
- `keys.go` Enter ハンドラ — 直接呼び出し

**目標**: 右ペイン制御を `switchRightPane(sid, focusRight bool)` に一本化する。

### `switchRightPane` の責務

```
switchRightPane(sid, focusRight):
  snap = manager.GetSession(sid).Snapshot()
  
  if snap.Display == DisplayNone:
    manager.FocusSession(sid)       // tmux select-window <sessionID>
  else:
    preview.WriteSelection(sid)     // IPC: __preview__ に選択を通知
    manager.FocusPreviewWindow()    // tmux select-window __preview__
  
  if focusRight:
    ghostty.FocusRight()            // Ghostty ペインフォーカスを右に
```

### 呼び出し元マッピング

| 呼び出し元 | `focusRight` | 備考 |
|---|---|---|
| `updateSelected()` | `false` | カーソル移動・display 変化の自動追従 |
| `Enter` on DisplayNone | `true` | 明示的に右ペイン操作 |
| `sessionCreatedMsg` (n) | `true` | 新規セッション |
| `sessionResumedMsg` (r/Enter on stopped) | `true` | 再開 |
| `sessionForkedMsg` (f) | `true` | フォーク |

### `updateSelected()` のトリガー条件

```
IF idChanged (カーソル移動):
  → switchRightPane(newID, false)
  
IF !idChanged && newDisplay != oldDisplay (同セッションの状態変化):
  → switchRightPane(sameID, false)
  ※ kill → preview, resume → tmux window の自動切替
```

---

## 現状の問題点

### 問題 1: `j/k` で running セッションに移動時、tmux window は切り替わるが Ghostty の pane indicator が更新されない

`updateSelected()` は `FocusSession()` を呼ぶが `FocusRight()` は呼ばない。
→ 上記設計では `focusRight=false` のため意図通り（`j/k` ではフォーカスを移さない）。
ただし「tmux select-window したのに Ghostty のアクティブペイン表示が右にならない」という **見た目の問題** がある。これは Ghostty の仕様: tmux select-window は内容を切り替えるが、Ghostty のフォーカス ring は変わらない。

**判断**: `j/k` でフォーカスを移すかはユーザー決定事項。現状は「移さない」で統一。

### 問題 2: `sessionResumedMsg` で `FocusSession()` が呼ばれない

現在の実装:
```go
// sessionResumedMsg ハンドラ
if m.splitMode {
    cmds = append(cmds, func() tea.Msg {
        _ = ghostty.FocusRight()   // FocusSession は呼ばない
        return nil
    })
}
// updateSelected の差分検知で FocusSession が呼ばれると期待しているが、
// refreshSessions() が呼ばれないため updateSelected は実行されない。
```

`sessionResumedMsg` では `refreshSessions()` を呼ばないため `updateSelected()` が動かない。
`FocusSession()` は次の `SessionRefreshMsg`（Manager onChange）まで呼ばれない。

→ `switchRightPane` に一本化すれば `FocusSession()` + `FocusRight()` が一発で解決。

### 問題 3: 新規/フォーク時の `idChanged=false` 問題

`sessionCreatedMsg` / `sessionForkedMsg` が `m.selectedID = msg.sessionID` を
`refreshSessions()` の前に設定するため、`updateSelected()` で `oldID == newID` となり
`idChanged = false`。前のセッションも DisplayNone だった場合、差分がなく切替が起きない。

→ 現在は `splitModeFocusCmd` で回避しているが、`switchRightPane` に統合することで整理できる。

### 問題 4: WriteSelection と FocusPreviewWindow のレース

```go
go func() {
    _ = preview.WriteSelection(dataDir, sid)  // ファイル書き込み
    _ = mgr.FocusPreviewWindow()              // すぐに tmux select-window
}()
```

`FocusPreviewWindow()` が先に実行されて preview プロセスが IPC を読む前に
画面が表示される可能性がある。ただし実際には preview は fsnotify で非同期に
反応するため、表示切替後に内容が更新されるだけで UX 上は許容範囲。

---

## 実装計画

### Step 1: `switchRightPane` メソッドを追加

```go
// internal/tui/model.go

// switchRightPane updates the right tmux pane to reflect the current state of
// the given session. If focusRight is true, Ghostty focus is also moved to the
// right pane (e.g. after an explicit user action like Enter/n/r/f).
func (m *Model) switchRightPane(sid session.DeckSessionID, focusRight bool) tea.Cmd {
    if !m.splitMode {
        return nil
    }
    mgr := m.manager
    dataDir := m.config.DataDir
    return func() tea.Msg {
        var display session.DisplayChannel
        if sess := mgr.GetSession(sid); sess != nil {
            snap := sess.Snapshot()
            display = snap.Display
        }
        if display == session.DisplayNone {
            _ = mgr.FocusSession(sid)
        } else {
            if err := preview.WriteSelection(dataDir, sid); err != nil {
                debuglog.Printf("[switchRightPane] preview IPC: %v", err)
            }
            _ = mgr.FocusPreviewWindow()
        }
        if focusRight {
            _ = ghostty.FocusRight()
        }
        return nil
    }
}
```

### Step 2: 呼び出し元を `switchRightPane` に統一

**`updateSelected()`**:
```go
if m.splitMode && (idChanged || newDisplay != oldDisplay) {
    cmds = append(cmds, m.switchRightPane(sid, false))
}
```
※ `updateSelected()` は `tea.Cmd` を返すよう変更が必要（現在 `void`）。

**`sessionCreatedMsg` / `sessionForkedMsg`**:
```go
if m.tmuxMode {
    cmds = append(cmds, m.switchRightPane(msg.sessionID, true))
}
```
→ `splitModeFocusCmd` は削除。

**`sessionResumedMsg`**:
```go
if m.tmuxMode {
    focusRight := m.splitMode
    sid := m.selectedID
    cmds = append(cmds, m.switchRightPane(sid, focusRight))
}
```

**`handleListKey` Enter on DisplayNone**:
```go
if display == session.DisplayNone {
    return m.switchRightPane(m.selectedID, m.splitMode)
}
```

### Step 3: `updateSelected()` の戻り値を `tea.Cmd` に変更

現在 `void` の `updateSelected()` を `tea.Cmd` 返しに変えるか、
`m.pendingCmd` フィールドで蓄積する方式を採る。

**推奨**: `updateSelected() tea.Cmd` に変更し、呼び出し元で `cmds = append(cmds, m.updateSelected())` とする。

---

## 非 split mode（tmux mode のみ、Ghostty 分割なし）との差分

| 操作 | split mode | tmux mode only |
|---|---|---|
| カーソル移動 | `tmux select-window` のみ | `tmux select-window` のみ |
| Enter 起動中 | `tmux select-window` + `FocusRight()` | `tmux select-window` のみ |
| Enter 停止中 / r | resume + `FocusRight()` | resume のみ |
| n / f | `FocusRight()` あり | `FocusRight()` なし |

`switchRightPane` の `focusRight` 引数で自然に吸収できる。

---

## 関連ファイル

| ファイル | 役割 |
|---|---|
| `internal/tui/model.go` | `updateSelected()`, `switchRightPane()`, msg ハンドラ |
| `internal/tui/keys.go` | Enter/n/r/f/x キーハンドラ |
| `internal/session/manager.go` | `FocusSession()`, `FocusPreviewWindow()` |
| `internal/session/backend_tmux.go` | `Focus()` 実装 |
| `internal/preview/ipc.go` | WriteSelection / WatchSelection |
| `internal/ghostty/split_darwin.go` | `FocusRight()`, `FocusLeft()` |
