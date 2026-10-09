# セッション ID 管理

## ID の種類と関係

```
┌─────────────────────────────────────────────────────┐
│ claude-deck Session                                  │
│  ID: "81f7f486f1df6345"  (deck 内部、不変)           │
│                                                      │
│  SessionChain: ["0c0a0bb5-...", "fbbc0487-..."]      │
│    (Claude Code UUID の履歴、古い順。末尾が現在)     │
│                                                      │
│  環境変数: CLAUDE_DECK_SESSION_ID=81f7f486f1df6345   │
└─────────────────────────────────────────────────────┘
         ↕ (環境変数の deck ID で store の行を特定)
┌─────────────────────────────────────────────────────┐
│ Claude Code Session                                  │
│  session_id: "fbbc0487-..."                          │
│  JSONL: ~/.claude/projects/<proj>/fbbc0487-....jsonl │
└─────────────────────────────────────────────────────┘
```

deck-status プラグインは `CLAUDE_DECK_SESSION_ID` を使って `claude-deck hook session-start --session <ID>` を実行する。Claude Code のセッション ID を知らなくても、store の行を直接特定できる。

## /clear による ID 変遷

```
初期状態:
  SessionChain = ["aaa"]

/clear 実行 (SessionStart source=clear, session_id="bbb"):
  SessionChain = ["aaa", "bbb"]    ← 末尾に追加

プロセス終了 (bbb の JSONL に会話がない):
  → "bbb" は resume 不可
  → SessionChain = ["aaa"]         ← 末尾を外す
```

末尾を外す処理は `applyExited` が行う。ただし、外した後の末尾 ("aaa") を別の deck セッションが持っているときは外さない。

## 重複防止

同じ Claude Code セッションを 2 つの deck セッションが指さないようにする。

- Discovery は、追跡中の全セッションの SessionChain（過去の ID を含む）を known として扱い、`/clear` 前の ID を外部セッションとして取り込まない。SessionChain は store に保存されるので、再起動しても残る
- `Reload` は、外部セッション（メモリのみ）の SessionChain の ID を deck セッションが持つようになったら、その外部セッションを消す。hook で ID が届く前に Discovery が取り込んでいた場合に起こる
- `LoadExisting` は、同じ ID を持つ deck セッションが store に複数あれば、SessionChain が長いほうを残して他を削除する
- `applyExited` は、上記のとおり他セッションと ID が衝突する巻き戻しをしない

## SessionStart の source

`applySessionStart` の動作。

| source | 動作 |
|---|---|
| `startup` / `resume` / `fork` | SessionChain が空のときだけ ID を追加する。再開で届く ID はチェーンの末尾と同じなので無視する |
| `clear` / `compact` | 末尾と異なる ID なら追加する |
| その他 | 無視する |
