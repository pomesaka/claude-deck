# Claude Code CLI 内部仕様メモ

claude-deck の実装参考用。minified された `cli.js` と JSONL 出力の解析結果に基づく。

## Permission (ツール承認)

### 内部モデル

Permission の判定結果は `behavior` で表現される:

- `"allow"` — 許可
- `"deny"` — 拒否
- `"ask"` — ユーザーに確認

ルールは3種のテーブルで管理:

```
alwaysAllowRules  — 自動許可ルール（セッション単位 or CLI引数）
alwaysDenyRules   — 自動拒否ルール
alwaysAskRules    — 常に確認ルール
```

### Permission Mode

`--permission-mode` フラグで全体の挙動を制御:

| モード | 挙動 |
|--------|------|
| `default` | 標準（ツールごとに確認） |
| `acceptEdits` | ファイル編集は自動承認 |
| `plan` | プラン承認が必要 |
| `dontAsk` | 全ツール自動承認 |
| `bypassPermissions` | 全権限チェックをスキップ |
| `delegate` | 委譲モード |

### PTY 上の UI 操作

Permission プロンプトの選択肢は Ink の Select コンポーネントで描画される。

**キー操作:**
- `↓` / `Ctrl+N` — 次の選択肢
- `↑` / `Ctrl+P` — 前の選択肢
- `Space` — フォーカス中の選択肢をトグル/選択
- **数字キー (1-9)** — 該当番号の選択肢を直接選択＆送信（1キーストロークで完了）

PTY 経由で応答する場合、数字キーが最もシンプル:

```go
p.ptmx.Write([]byte("1"))  // 1番目の選択肢（通常 "Yes, allow"）
p.ptmx.Write([]byte("2"))  // 2番目の選択肢
```

### PermissionRequest Hook

hooks 設定で `PermissionRequest` イベントを処理できる。
フックの出力で `behavior: "allow"` / `"deny"` を返すと、ユーザー確認なしに自動判定される。
claude-deck からの自動応答はフック経由が PTY 入力より確実。

### JSONL 上の記録

Permission 関連のイベントは JSONL に記録されない（ユーザーの操作はツール実行後の `tool_result` として記録される）。

## AskUserQuestion (質問)

### ツール定義

Claude が `AskUserQuestion` ツールを呼ぶと、ユーザーに多肢選択の質問が表示される。

```typescript
interface AskUserQuestionInput {
  questions: Array<{
    question: string;         // 質問文
    header: string;           // ラベル（最大12文字）
    options: Array<{          // 2-4個の選択肢
      label: string;          // 選択肢テキスト
      description: string;    // 説明
    }>;
    multiSelect: boolean;     // 複数選択可否
  }>;  // 1-4個の質問
  answers?: Record<string, string>;  // 回答
}
```

- 選択肢は自動的に「Other」（自由テキスト入力）が追加される
- `multiSelect: true` の場合、Space で複数トグル可能

### PTY 上の UI 操作

AskUserQuestion も同じ Select コンポーネントを使用:

- **数字キー** — 選択肢を直接選択
- **Tab** — 「Other」テキスト入力に切り替え
- テキスト入力モードでは通常のキー入力 + Enter で送信

### JSONL 上の記録

```jsonl
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"AskUserQuestion","input":{"questions":[...]}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"...","content":"選択された回答テキスト"}]}}
```

`tool_use` エントリには質問と選択肢の全情報が含まれる。
`tool_result` にはユーザーが選択した回答が入る。

## Subagent (Task ツール)

### 起動

Claude が `Task` ツールを呼ぶとサブエージェントが起動される。

```typescript
interface AgentInput {
  description: string;        // 短い説明 (3-5語)
  prompt: string;             // タスク内容
  subagent_type: string;      // エージェント種別
  model?: "sonnet" | "opus" | "haiku";
  resume?: string;            // 前回のエージェントIDで再開
  run_in_background?: boolean;
  max_turns?: number;
  mode?: "acceptEdits" | "bypassPermissions" | "default" | "delegate" | "dontAsk" | "plan";
}
```

### JSONL の構造

メインセッションの JSONL:
```
~/.claude/projects/<project-dir>/<session-uuid>.jsonl
```

サブエージェントの JSONL:
```
~/.claude/projects/<project-dir>/<session-uuid>/subagents/agent-<id>.jsonl
```

**メインの JSONL に記録されるもの:**
- `tool_use`: Task の `description` と `prompt`（サブエージェントへの指示）
- `tool_result`: サブエージェントの**最終結果のみ**（要約テキスト）

**サブエージェント内部の動き（Read/Grep/Edit 等）はメインの JSONL に記録されない。**
リアルタイムで Explore 中の動きを見たい場合は、サブエージェントの JSONL も読む必要がある。

### ディレクトリ構造

JSONL 削除時にサブエージェントのディレクトリは残る点に注意:

```
<project-dir>/
  <session-uuid>.jsonl          ← メインセッション（削除対象）
  <session-uuid>/               ← ディレクトリ（削除されない）
    subagents/
      agent-<id>.jsonl          ← サブエージェントログ
```

### 14日間の Discovery ウィンドウ

`ListAllSessions` は `filepath.Glob("*/*.jsonl")` でスキャンし、最終更新が14日以内のファイルのみ対象。
サブエージェントファイルは `subagents/` パスを含むため自動除外される。

## Team / Swarm モード（参考）

チームモードでは **mailbox** によるファイルベース IPC を使用:

```
~/.claude/teams/<team-hash>/inboxes/<agent-hash>/
```

- リーダー↔ワーカー間で Permission 同期
- `createPermissionRequestMessage` / `createPermissionResponseMessage` でメッセージ交換
- `ShutdownRequest` / `ShutdownApproved` / `ShutdownRejected` でライフサイクル管理

claude-deck が直接使う想定はないが、ファイルベース IPC の設計パターンとして参考になる。
