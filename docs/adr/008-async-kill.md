# ADR-008: `x` キーによるセッション削除の非同期化

## ステータス

Accepted

## コンテキスト

`x` キーでセッションを終了する `killSelected` は、`Manager.Kill()` を Bubble Tea の `Update()` 内で同期実行していた。`Kill()` 内の `cleanupWorkspace` は `jj workspace forget` と `os.RemoveAll(wsRootPath)` を実行するが、`node_modules` のような大量ファイルを含むワークスペースでは後者が数秒かかる。その間 Bubble Tea のイベントループがブロックされ、TUI が完全にフリーズする。

Create / Resume / Fork は既に `tea.Cmd` クロージャ（goroutine）で非同期化されており、`killSelected` だけが同期パターンのまま残っていた。

## 決定

`killSelected` を `tea.Cmd` クロージャに変更し、完了時に `sessionKilledMsg` を返す非同期パターンに統一した。

```
x キー押下
 → killSelected() が tea.Cmd を返す（即座にリターン）
 → bubbletea が goroutine で Cmd を実行
   → Manager.Kill(sid)
     → StopProcess（高速: tmux kill-window）
     → watchProcess が notifyChange → SessionRefreshMsg → UI 更新（ほぼ即座）
     → cleanupWorkspace（低速: jj forget + os.RemoveAll）
   → sessionKilledMsg を返す
 → Update() がステータスバーを更新
```

`m.killing bool` フラグを追加して、`sessionKilledMsg` 受信までの間の二重発火を防ぐ。Resume / Fork にはこのフラグがないが、それらは Manager レイヤーのステータスチェックで冪等に処理されかつ完了が高速なため、競合リスクが低い。

`sessionKilledMsg` ハンドラでは `refreshSessions()` を明示的に呼ばない。`Kill()` 末尾の `notifyChange()` がデバウンスループ経由で `SessionRefreshMsg` を発行するためである。Kill 失敗時（`StopProcess` エラーで早期 return）は `notifyChange` が呼ばれないが、その場合セッション状態は変化していないため問題ない。

## 結果

**良い点**
- `node_modules` のような大容量ワークスペースでも UI がフリーズしなくなった。
- Create / Resume / Fork と同じ `tea.Cmd` 非同期パターンに揃い、一貫性が向上した。
- 即座に「セッション終了中...」が表示されるため、ユーザーが操作結果を認識できる。

**悪い点**
- `killing` フラグが Model に追加され、状態が増えた。

**却下した代替案**
- `Manager.Kill()` を「fast path」と「slow path」に分割し、ディレクトリ削除だけを別 goroutine で行う案。Kill のセマンティクスが複雑になり、エラー報告のタイミングが曖昧になるため却下。
- `selectedID` をクリアして二重発火を防ぐ案。カーソル位置が即座に消えて UX が不自然になるため却下。
