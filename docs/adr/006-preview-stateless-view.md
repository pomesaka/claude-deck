# ADR-006: Preview サブプロセスを stateless view に変更

ステータス: 採用済み

## コンテキスト

claude-deck は main プロセスと preview サブプロセス (`claude-deck --preview`) の 2 プロセス構成で、tmux `__preview__` window に JSONL ログを表示する。旧設計では両プロセスとも `session.Manager` をフルに持ち、IPC では `DeckSessionID` を 1 行テキストで渡していた。

この設計から 3 つの構造的問題が生じた:

1. **DeckSessionID 同期ずれ**: main 起動後に `n` で作成されたセッションは、preview の `handleNewFile` 経路で別の `DeckSessionID`（B）で取り込まれる。main が `WriteSelection(A)` を送ると preview の `GetSession(A)` が nil を返し、「セッションを選択してください」と表示される（`x` kill 後のプレビューが出ないバグの根本原因）。

2. **Ghost エントリ**: B がメモリに残り続ける（UX 上は不可視だが設計上の負債）。

3. **責務の二重化**: preview は「main が選んだセッションの JSONL を描画する view」でしかないのに、main と同じ discovery パイプライン（`LoadExisting` / `DiscoverExternalSessions` / `handleNewFile`）を持っていた。

## 決定

IPC プロトコルを `DeckSessionID` の文字列から `PreviewSpec` JSON に変更し、preview サブプロセスから `session.Manager` を完全に除去した。

### PreviewSpec（`internal/preview/ipc.go`）

main 側で全情報を解決して渡す:
- セッションのメタデータ（Name, RepoName, WorkspacePath, ClaudeSessionID, PriorClaudeIDs, Status, Display, CurrentTool など）
- JSONL パス（`JSONLPath`, `PriorJSONLPaths`）— main の `usage.Reader.ResolveSessionPath` で解決済み

preview は `DeckSessionID` をルックアップキーとして使わず、渡された情報をそのまま描画する。

### main 側の変更

- `buildPreviewSpec()` で `selectedSnap` と `Manager.ResolveJSONLPaths` から `PreviewSpec` を構築
- `doSwitchRightPane` で `WriteSpec` を呼ぶ
- `SessionRefreshMsg` ハンドラで選択中セッションの状態変化時に spec を再 publish — kill 時の DisplayNone → DisplayJSONL 遷移がリアルタイムに届く

### preview 側の変更

- `session.Manager` を削除。`runPreview` は debuglog init / config load / signal handler / ctx / `WatchSpec` / `p.Run()` のみ。
- `PreviewModel` は `PreviewSpec` を state として持ち、`previewStreamer` で自前 JSONL ストリーミングを行う。
- `previewStreamer` は `Manager.StreamSession` と同じアルゴリズム（ReadTail + RunFrom + prior paths prefix）を使い、goroutine が自身のチャネルを close してリスナーに終了を通知する clean なパターンで実装。

## 結果

### 良い点

- `DeckSessionID` 同期ずれが構造的に発生不可能になった（preview が DeckSessionID をルックアップキーとして使わない）
- Ghost B セッションが生まれなくなった（preview が `handleNewFile` を呼ばない）
- preview サブプロセスのメモリ使用量が大幅に減少（Manager / store / fsnotify watcher が不要）
- 責務が明確に分離: main = session authority, preview = read-only renderer
- `SessionRefreshMsg` 経由の spec re-publish により、kill 後の状態遷移が即座に preview に届く

### 削除されたコード

- `runPreview` から: `store.New`, `NewManager`, `LoadExisting`, `HydrateFromJSONL`, `DiscoverExternalSessions`, `StartFileWatcher`, `SetOnChange`, `StartNotifyLoop`
- `PreviewModel` から: `manager` フィールド, `refreshSnap`, Manager 経由の `GetStructuredLogs`

### 却下した代替案

**A案 (lazy SyncNewFromStore)**: `PreviewSelectionMsg` で `GetSession` が miss したとき `SyncNewFromStore` を呼ぶ最小修正。Ghost B 問題が残り、preview が main と同じ discovery パイプラインを持つ構造的違和感も残る。

**DeckSessionID を map key として使い続けつつ sync 強化**: 問題の原因（2 プロセスが独立して ID を生成する）を残したまま帯域的対処を重ねる方向。長期的に複数の同期バグの温床になる。
