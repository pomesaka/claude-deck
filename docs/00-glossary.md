# ドメイン用語集

claude-deck のコードと会話で使われる用語の定義。コードを読む前にここで概念を掴む。

> 用語の追加・変更時はこのファイルとコードを同時に更新すること。
> 対応する型がある場合は `パッケージ.型名` で示す。

## セッション

### Session

Claude Code との1つの対話セッション。claude-deck の中心概念。

Claude Code のプロセスライフサイクル、対話履歴、作業ディレクトリを追跡する。Session は複数のデータソースから状態を投影 (→ Projection) して構築される。

**型**: `session.Session`

### DeckSessionID

claude-deck が内部で割り振るセッション識別子。ランダム hex 文字列。Session の一生を通じて不変。

**型**: `session.DeckSessionID`

### Alias

利用者かセッション自身が `claude-deck alias` で付ける表示用の名前。TUI の一覧では、セッション名の代わりに出る。セッション名は tmux のウィンドウ名と jj ワークスペース名を兼ねていて変えられないので、表示だけを別に持つ。使えるのは英数字と `-` `_` `.` で、一意である必要はない。CLI での指定には使えない。

**フィールド**: `Session.Alias`

### ClaudeSessionID

Claude Code 側が割り振る UUID。`/clear` のたびに新しい ID が生成される。SessionChain の末尾が現在の ClaudeSessionID。

**型**: `session.ClaudeSessionID`

### SessionChain

1つの Session が経験した ClaudeSessionID の履歴 (古い順)。`/clear` や compact のたびに末尾に新 ID が追加される。`CurrentRuntimeID()` は末尾、`PriorRuntimeIDs()` はそれ以前を返す。

**フィールド**: `Session.SessionChain []ClaudeSessionID`

### ForkedFrom

フォークで作った Session の分岐元の ClaudeSessionID。別の Session の SessionChain の要素を指す。フォークでなければ空。`claude-deck tree` は、SessionChain と ForkedFrom から ClaudeSessionID の木を組み立てる（[ADR 012](adr/012-fork-lineage.md)）。

**フィールド**: `Session.ForkedFrom ClaudeSessionID`

## 状態モデル

### Status

セッションの細粒度な実行状態。7値の列挙型。

| 値 | 意味 | 遷移先 |
|----|------|--------|
| Idle | プロセス起動済みだが Claude が処理中でない (Hook turn.complete) | Running, Completed, Error |
| Running | Claude が思考/実行中 (Hook turn.start / tool.call) | Idle, SubagentRunning, Waiting*, Completed, Error |
| SubagentRunning | メインのターンは終わったが、バックグラウンドのサブエージェントが動いている (Hook turn.complete) | Running, Idle, Waiting*, Completed, Error |
| WaitingApproval | ツール承認待ち (Hook PermissionRequest) | Running, Idle, Completed, Error |
| WaitingAnswer | ユーザー質問待ち (Hook PermissionRequest / AskUserQuestion の tool.call) | Running, Idle, Completed, Error |
| Completed | プロセス終了 (ウィンドウのコマンドの `hook exited` / close / ウィンドウ消失の検知) | Idle (Resume 経由) |
| Error | プロセス異常終了 / ディレクトリ消失 | Idle (Resume 経由) |
| Unmanaged | JSONL から発見された外部セッション | (遷移なし) |

**型**: `session.Status`

### DisplayChannel

右ペインに何を表示するかの投影。Status から導出される (永続化しない)。

| 値 | 条件 | 表示内容 |
|----|------|----------|
| DisplayTmux | 未終了 (Idle / Running / SubagentRunning / Waiting*) | セッションの tmux ウィンドウ |
| DisplayJSONL | 終了済み (Completed / Error) または外部 (Unmanaged) | preview ウィンドウの JSONL 構造化ログ |

**型**: `session.DisplayChannel`

## データアーキテクチャ

### データソース

Session の状態を構成するデータソース。各ソースが Session の特定のフィールドを「所有」する。

| ソース | 所有フィールド | 更新タイミング |
|--------|---------------|---------------|
| **Store** | ID, Name, RepoPath, SessionChain, Status, PID, LastJJRevision, LastJJParentRevision | 信頼できる唯一の情報源。TUI・CLI・hook コマンドが SQLite に書き、TUI は `Reload` で読む |
| **JSONL** | Prompt, PermissionMode, StartedAt, LastActivity | Claude Code が JSONL に書き込み時 |
| **jj** | BookmarkName | TUI が 5 秒ごとに読む |
| **Hook** | Status 遷移, SessionChain 追加 | deck-status プラグインが `claude-deck hook` で store に書く。TUI が `WatchStore` で検知する |

### LastJJRevision / LastJJParentRevision

`x`（Kill）でセッションを終了する直前に保存される jj の revision 情報。常にペアで更新される。

| フィールド | 内容 |
|---|---|
| `LastJJRevision` | Kill 時の working copy（`@`）の change_id |
| `LastJJParentRevision` | Kill 時の working copy の親（`@-`）の change_id（fallback） |

`r`（Resume）でワークスペースを再作成する際に、`jj new trunk()` ではなくここに保存した revision から再開するために使われる（優先順: `@` → `@-` → `trunk()`）。`@` が空の場合は `jj workspace forget` で abandon されるため `@-` を fallback として保存する。詳細は ADR 009 参照。

**フィールド**: `Session.LastJJRevision`, `Session.LastJJParentRevision`

### Projection (投影)

複数のデータソースから Session の統一状態を構築するパターン。store の項目は `Reload` が、JSONL と jj の項目は Session の `Apply*` メソッド群 (`ApplyFileActivity`, `ApplyBookmark`) が書く。

### Snapshot

Session のロックフリーな読み取りコピー。TUI レンダリングは常に Snapshot を通じてデータにアクセスする。DisplayChannel などの導出フィールドも含む。

**型**: `session.Snapshot`

## インフラ

### deck-status プラグイン

claude-deck が起動する全セッションに `--plugin-dir` で渡す Claude Code プラグイン（`deckmod/`）。Claude Code のイベントを受けて `claude-deck hook` を実行し、Status 遷移と SessionChain 更新を store に書く。claude-deck の CLI の使い方を書いたスキルも同梱する。ユーザーが別途インストールする必要はない。

**関連**: `deckmod/`, [hooks.md](hooks.md)

### JSONL

Claude Code が `~/.claude/projects/<project>/<uuid>.jsonl` に書き出すセッションログ。対話のプロンプト、レスポンス、ツール実行を含む。claude-deck の一次データソース。

**関連**: `internal/usage/`

### Store

claude-deck 固有のメタデータ永続化。`~/.local/share/claude-deck/deck.db`（SQLite）に deck セッションを 1 行ずつ持つ。JSONL が「Claude Code の記録」、Store は「deck の記録」で、deck セッションの信頼できる唯一の情報源。外部セッション (Unmanaged) は持たない。

**関連**: `internal/store/`

### Workspace

jj (Jujutsu) のワークスペース機能で作成される隔離作業ディレクトリ。各セッションが独立したファイルシステム状態を持てる。

**関連**: `internal/jj/`
