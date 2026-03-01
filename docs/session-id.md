# セッション ID 管理

## ID の種類と関係

```
┌─────────────────────────────────────────────────────┐
│ claude-deck Session                                  │
│  ID: "81f7f486f1df6345"  (deck 内部、不変)           │
│                                                      │
│  ClaudeSessionID: "fbbc0487-..."  (Claude Code UUID) │
│  PreviousClaudeSessionID: "0c0a0bb5-..." (旧 UUID)  │
│                                                      │
│  環境変数: CLAUDE_DECK_SESSION_ID=81f7f486f1df6345   │
└─────────────────────────────────────────────────────┘
         ↕ (フックイベントで紐付け)
┌─────────────────────────────────────────────────────┐
│ Claude Code Session                                  │
│  session_id: "fbbc0487-..."                          │
│  JSONL: ~/.claude/projects/<proj>/fbbc0487-....jsonl │
└─────────────────────────────────────────────────────┘
```

## /clear による ID 変遷

```
初期状態:
  ClaudeSessionID = "aaa"
  PreviousClaudeSessionID = ""

/clear 実行:
  ClaudeSessionID = "bbb"         ← 新しい UUID
  PreviousClaudeSessionID = "aaa" ← 旧 UUID を保存
  oldSessionIDs["aaa"] = true     ← Discovery で除外

プロセス終了 (bbb の JSONL が空):
  → "bbb" に会話データがない = resume 不可
  → ClaudeSessionID = "aaa" に revert
  → PreviousClaudeSessionID = ""
```

## 重複防止メカニズム

### 問題

`/clear` 後に `oldSessionIDs` が失われると（再起動等）、
Discovery が旧 ID を外部セッションとして再インポートし、
2つの deck セッションが同じ Claude Code セッションを指す。

### 防御層

1. **LoadExisting**: ストアから `PreviousClaudeSessionID` を `oldSessionIDs` に復元
2. **Discovery known セット**: `ClaudeSessionID` と `PreviousClaudeSessionID` の両方をチェック
3. **handleNewFile**: 同上
4. **watchProcess revert**: `isClaudeIDClaimed()` で他セッションとの衝突をチェック

### oldSessionIDs の永続性

`oldSessionIDs` は runtime map（永続化されない）。
代わりに各セッションの `PreviousClaudeSessionID` がストアに永続化され、
`LoadExisting` 時にマップを再構築する。

## Discovery の除外ロジック

```go
// known に含まれるものは除外
known[s.ClaudeSessionID] = true
known[s.PreviousClaudeSessionID] = true

// oldSessionIDs (旧 ID) も除外
for id := range m.oldSessionIDs {
    known[id] = true
}
```

## --resume 時のイベントシーケンス

```
claude --resume fbbc0487-...
  → SessionStart {session_id: "transient-uuid", source: "startup"}
    deck: ClaudeSessionID = "transient-uuid" (一時的)
  → SessionStart {session_id: "fbbc0487-...", source: "resume"}
    deck: ClaudeSessionID = "fbbc0487-..." (正しい ID で上書き)
```

startup の一時 ID は resume イベントで即座に正しい ID に上書きされるため、
通常は問題にならない。
