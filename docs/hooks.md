# Claude Code フック連携

## 仕組み

Claude Code は `~/.claude/settings.json` の hooks 設定に基づいて、
セッションイベント発生時にシェルコマンドを実行する。

claude-deck は jq コマンドで stdin JSON からフィールドを抽出し、
`CLAUDE_DECK_SESSION_ID` 環境変数を注入して JSONL ファイルに追記する。

```json
{
  "hooks": {
    "SessionStart": [{"type": "command", "command": "jq -c '{...}' >> '<events-path>'"}],
    "SessionEnd":   [...],
    "Notification":  [...],
    "Stop":          [...]
  }
}
```

## イベント種別

### SessionStart

```json
{
  "session_id": "UUID",
  "source": "startup" | "resume" | "clear" | "compact",
  "claude_deck_session_id": "DECK_SESSION_ID"
}
```

- `startup`: claude CLI 起動時（一時的な ID が割り当てられる）
- `resume`: `--resume` で既存セッション復元時（正しい ID に上書き）
- `clear`: `/clear` 後に新セッション開始時
- `compact`: コンテキストコンパクション後

**注意**: `--resume` 起動時は `startup` → `resume` の順で2つ発火する。
`resume` の session_id が正しい ID なので、startup の ID は上書きされる。

### SessionEnd

```json
{
  "session_id": "UUID",
  "reason": "clear" | ...,
  "claude_deck_session_id": "DECK_SESSION_ID"
}
```

`/clear` 時のみ発火を確認。`reason` で識別。

### Notification

```json
{
  "session_id": "UUID",
  "notification_type": "permission_prompt" | "elicitation_dialog" | "idle_prompt"
}
```

- `permission_prompt`: ツール実行の承認待ち
- `elicitation_dialog`: ユーザーへの質問待ち
- `idle_prompt`: 入力待ち（タスク完了後）

### Stop

```json
{
  "session_id": "UUID"
}
```

Claude Code がタスクを完了して入力待ちに戻った時。

## ペアリングメカニズム

`/clear` や `compact` では SessionEnd → SessionStart のペアで発火する。
ペアリングは `CLAUDE_DECK_SESSION_ID` をキーに行う。

```
pendingEndEvents map[string]*hooks.Event  // key = ClaudeDeckSessionID

SessionEnd 受信:
  pendingEndEvents[ev.ClaudeDeckSessionID] = &ev

SessionStart (source=clear/compact) 受信:
  pendEnd = pendingEndEvents[ev.ClaudeDeckSessionID]
  delete(pendingEndEvents, ev.ClaudeDeckSessionID)
  → oldCSID = pendEnd.SessionID
  → newCSID = ev.SessionID
  → セッションの ClaudeSessionID を更新
```

## フック設定の管理

### EnsureHooks

起動時に `~/.claude/settings.json` を読み込み、フック設定を追加/更新。

- 初回: 既存設定をバックアップ (.bak)
- 既存フック: claude-deck のフックを末尾に追加（他ツールのフックは保持）
- 更新検知: イベントファイルパスが変わった場合に更新

### イベントファイル

`~/.local/share/claude-deck/claude-deck-events.jsonl`

起動時に truncate して古いイベントを破棄。
WatchEvents が fsnotify でファイル変更を監視し、新しい行のみ処理。

### truncate 後のオフセットリセット

ファイルが truncate された場合（サイズ < 現在のオフセット）、
オフセットを 0 にリセットして先頭から再読み込みする。
