# ADR-009: Runtime provider abstraction for Claude Code and Codex

## ステータス

Accepted

## コンテキスト

claude-deck は Claude Code セッション管理を前提に成長してきた。既存実装は以下を Claude Code 固有の前提として持っていた。

- 起動引数: `claude --resume`, `--fork-session`, `--agent`, `--permission-mode`
- transcript 配置: `~/.claude/projects/<project>/<uuid>.jsonl`
- status 更新: Claude Code plugin hooks (`SessionStart`, `Stop`, `PermissionRequest` など)
- session ID: Claude Code が発行する UUID

Codex CLI は同じ「対話型 coding agent」だが、CLI と transcript の形が異なる。

- 起動引数: `codex resume <id>`, `codex fork <id>`, `--cd`, `--ask-for-approval`
- transcript 配置: `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`
- status 更新: Claude Code hooks ではなく Codex JSONL の `event_msg` / `response_item`

この差分を Manager や TUI に直接分岐として広げると、今後 provider が増えるたびにセッションライフサイクルの中心が肥大化する。

## 決定

Claude Code 固有の前提を provider 境界に切り出す。

1. `internal/agentruntime.Runtime` が runtime-neutral な `StartRequest` を CLI 固有の `StartSpec` に変換する。
2. `session.RuntimeSessionID` を導入し、deck 内部では provider-neutral な runtime session ID として扱う。
3. `usage.Reader` に `TranscriptLayout` を持たせ、Claude / Codex の transcript discovery, session ID 解決, token/log projection を分ける。
4. `config.toml` の `[runtime] provider` で `claude` / `codex` を選択し、`buildManagerConfig` で Runtime と transcript reader を同時に選ぶ。
5. Claude Code hooks は Claude provider のみの realtime status input とする。Codex provider では JSONL tail から `RuntimeActivity` を抽出し、`Running` / `Idle` / `CurrentTool` に投影する。

## 結果

**良い点**

- Manager は「新規 / resume / fork」という lifecycle intent を扱い続け、CLI 固有の引数を知らない。
- TUI と store は deck session ID と runtime session ID の区別だけを知ればよく、Claude / Codex の transcript 配置を知らない。
- Codex 対応は既存 Claude Code のデフォルト挙動を変えずに opt-in できる。
- Codex の realtime status は hooks がなくても JSONL 書き込みから最低限投影できる。

**悪い点**

- Codex では Claude Code plugin hooks と同じ粒度の `WaitingApproval` / `WaitingAnswer` 検出はまだできない。
- `SessionChain` の `/clear` 相当更新は Claude hooks に依存しており、Codex では transcript が新規 rollout に切り替わる挙動を追加調査する必要がある。
- README や既存設計 docs には Claude Code 前提の記述が残っており、今後段階的に agent runtime 前提へ更新する必要がある。

**却下した代替案**

- Manager に `if provider == codex` の分岐を直接追加する案。短期的には簡単だが、起動引数・transcript・status 更新の3つの差分が Manager に漏れて責務が混ざるため却下。
- Codex transcript を Claude JSONL 互換のファイルへ変換してから読む案。変換ファイルのライフサイクルが増え、一次データとの同期問題が生じるため却下。
- Codex は起動だけ対応し、status/log は未対応にする案。dashboard としての価値が大きく落ち、ユーザーが「動いているが見えない」状態になるため却下。
