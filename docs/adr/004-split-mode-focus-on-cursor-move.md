# ADR-004: split モードでカーソル移動時も DisplayNone セッションの実ウィンドウを表示する

## ステータス

Accepted

## コンテキスト

split モード（Ghostty + tmux）では右ペインの表示制御に2種類の操作がある:

- **カーソル移動** (`focusRight=false`): ユーザーが一覧を閲覧中
- **明示的操作** (`focusRight=true`): Enter / 新規 / 再開 / fork

当初の設計では「カーソル移動は常に `__preview__`（JSONL viewer）を表示する」としていた。
停止中セッションでは会話履歴を表示できるため有用。
しかし `DisplayNone`（実行中・tmux ホスト）セッションの場合、`__preview__` は `syncViewport` でビューポートを空にする仕様だったため（`Display == DisplayNone` → `logViewport.SetContent("")`）、ヘッダー数行しか表示されず残り全体がターミナルの黒背景になっていた。

```
doSwitchRightPane(sid, focusRight=false)
  └─ IsSplit() = true
       └─ focusRight=false なので __preview__ を表示  ←── DisplayNone でも黒画面
```

ユーザーから「カーソルを当ててもウィンドウが真っ黒。Enter を押すと表示される」と報告。

## 決定

`doSwitchRightPane` の split モード分岐を `focusRight` ではなく `display` で最初に分岐する:

- `display == DisplayNone`（実行中）: `focusRight` にかかわらず `FocusSession(sid)` を呼ぶ。`focusRight=true` のときのみ追加で `ghostty.FocusRight()` を呼ぶ。
- `display != DisplayNone`（停止中）: 従来通り `__preview__` を表示。

```go
if display == session.DisplayNone {
    _ = m.manager.FocusSession(sid)
    if focusRight {
        _ = ghostty.FocusRight()
    }
    return
}
// 非稼働 → __preview__ (JSONL)
```

## 結果

**良い点**:
- カーソル移動で実行中セッションの tmux ウィンドウが即座に見える
- Enter との差は Ghostty フォーカスを右ペインに移すかどうかのみで直感的
- `__preview__` の JSONL 表示は停止中セッションに限定されるため、ユースケースが明確

**悪い点**:
- 複数の実行中セッションをカーソルで素早く移動すると tmux ウィンドウが高速に切り替わる。ただし `FocusSession` は `tmux select-window`（~0ms）のため視覚的ちらつきは許容範囲

**却下した代替案**:
- `__preview__` で `DisplayNone` のときも JSONL ビューポートを表示する: 実行中セッションのターミナル出力を見たいというユーザーの期待に応えられない
