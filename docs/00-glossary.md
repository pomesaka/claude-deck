# ドメイン用語集

claude-deck のコードと会話で使われる用語の定義。コードを読む前にここで概念を掴む。

> 用語の追加・変更時はこのファイルとコードを同時に更新すること。
> 対応する型がある場合は `パッケージ.型名` で示す。

## セッション

### Session

Claude Code との1つの対話セッション。claude-deck の中心概念。

Claude Code のプロセスライフサイクル、トークン使用量、対話履歴、作業ディレクトリを追跡する。Session は複数のデータソースから状態を投影 (→ Projection) して構築される。

**型**: `session.Session`

### DeckSessionID

claude-deck が内部で割り振るセッション識別子。ランダム hex 文字列。Session の一生を通じて不変。

**型**: `session.DeckSessionID`

### ClaudeSessionID

Claude Code 側が割り振る UUID。`/clear` のたびに新しい ID が生成される。SessionChain の末尾が現在の ClaudeSessionID。

**型**: `session.ClaudeSessionID`

### SessionChain

1つの Session が経験した ClaudeSessionID の履歴 (古い順)。`/clear` や compact のたびに末尾に新 ID が追加される。`CurrentClaudeID()` は末尾、`PriorClaudeIDs()` はそれ以前を返す。

**フィールド**: `Session.SessionChain []ClaudeSessionID`

## 状態モデル

### Status

セッションの細粒度な実行状態。7値の列挙型。

| 値 | 意味 | 遷移先 |
|----|------|--------|
| Idle | PTY 起動済みだが Claude が処理中でない | Running, Completed, Error |
| Running | スピナー検知中 (Claude が思考/実行中) | Idle, Waiting*, Completed, Error |
| WaitingApproval | ツール承認待ち (Hook Notification) | Running, Idle, Completed, Error |
| WaitingAnswer | ユーザー質問待ち (Hook Notification) | Running, Idle, Completed, Error |
| Completed | プロセス正常終了 | Idle (Resume 経由) |
| Error | プロセス異常終了 / ディレクトリ消失 | Idle (Resume 経由) |
| Unmanaged | JSONL から発見された外部セッション | (遷移なし) |

**型**: `session.Status`

### SessionPhase

RunningProcess と Status から導出される粗粒度のライフサイクル段階。TUI や Manager の条件分岐を単純化するための概念。

| 値 | 意味 | 導出条件 |
|----|------|----------|
| Active | プロセスが生存中、または未完了 | Status=Unmanaged 以外 かつ (IsTerminal でない OR RunningProcess あり) |
| Archived | 完了済み (Completed/Error) でプロセスなし | IsTerminal && RunningProcess == nil |
| External | JSONL から発見、deck 未起動 | Status=Unmanaged |

**型**: `session.SessionPhase`

### RunningProcess

実行中プロセスへのハンドルを束ねた Value Object。`display` フィールドの nil/non-nil で Embedded/External を区別する。  
Session は `atomic.Pointer[RunningProcess]` として保持し、プロセス起動時に `AttachProcess(pid, display)` でセット、終了時に `DetachProcess()` でクリアする。

- `display != nil` → **Embedded**: claude-deck が PTY を所有し、エミュレータで出力をキャプチャ
- `display == nil` → **External**: 外部ターミナルが PTY を所有し、claude-deck はメタデータのみ追跡
- `RunningProcess == nil` → プロセス未起動 or 終了済み

**型**: `session.RunningProcess`

### HostingMode

detail pane に「誰がターミナルを所有しているか」を伝える導出値。RunningProcess から決まる (永続化しない)。

| 値 | 意味 | 導出条件 |
|----|------|----------|
| HostEmbedded | claude-deck が PTY を所有 | RunningProcess.display != nil |
| HostExternal | 外部ターミナルが PTY を所有、またはプロセスなし | RunningProcess == nil or display == nil |

**型**: `session.HostingMode`

### DisplayChannel

detail pane に何を表示するかの投影。RunningProcess から導出される (永続化しない)。

| 値 | 条件 | 表示内容 |
|----|------|----------|
| DisplayPTY | RunningProcess あり + display あり (Embedded) | PTYDisplay のリアルタイム画面 |
| DisplayNone | RunningProcess あり + display なし (External) | 「外部ターミナルで表示中」プレースホルダ |
| DisplayJSONL | RunningProcess なし | JSONL 構造化ログ |

**型**: `session.DisplayChannel`

## データアーキテクチャ

### DataSource

Session の状態を構成する4つのデータソース。各ソースが Session の特定のフィールドを「所有」する。

| ソース | 所有フィールド | 更新タイミング |
|--------|---------------|---------------|
| **Store** | ID, Name, RepoPath, SessionChain, Status, PID, BookmarkName, LastJJRevision, LastJJParentRevision | セッション作成・更新時に JSON 永続化 |
| **JSONL** | Prompt, PermissionMode, StartedAt, LastActivity, TokenUsage | Claude Code が JSONL に書き込み時 |
| **Hook** | Status 遷移, SessionChain 追加 | Claude Code フックイベント発火時 |
| **PTY** | LogLines, CurrentTool, Status (Running via spinner) | PTY 出力受信時 |

**型**: `session.DataSource`

### LastJJRevision / LastJJParentRevision

`x`（Kill）でセッションを終了する直前に保存される jj の revision 情報。常にペアで更新される。

| フィールド | 内容 |
|---|---|
| `LastJJRevision` | Kill 時の working copy（`@`）の change_id |
| `LastJJParentRevision` | Kill 時の working copy の親（`@-`）の change_id（fallback） |

`r`（Resume）でワークスペースを再作成する際に、`jj new trunk()` ではなくここに保存した revision から再開するために使われる（優先順: `@` → `@-` → `trunk()`）。`@` が空の場合は `jj workspace forget` で abandon されるため `@-` を fallback として保存する。詳細は ADR 009 参照。

**フィールド**: `Session.LastJJRevision`, `Session.LastJJParentRevision`

### Projection (投影)

複数の DataSource から Session の統一状態を構築するパターン。Session の `Apply*` メソッド群 (`ApplyJSONLTokens`, `ApplyFileActivity`, `ApplyBookmark`) が各ソースからの更新を正規化された方法で適用する。

### Snapshot

Session のロックフリーな読み取りコピー。TUI レンダリングは常に Snapshot を通じてデータにアクセスする。Status, Phase, DisplayChannel 等の導出フィールドも含む。

**型**: `session.Snapshot`

## PTY 表示

### PTYDisplay

PTY エミュレータの表示インフラをカプセル化した構造体。仮想端末 (vt.Emulator)、displayCache、scrollback、カーソル追跡を管理する。

- RunningProcess.display に格納される。Embedded セッションのみ non-nil
- External セッションでは RunningProcess.display == nil (型レベルで「表示インフラなし」を保証)
- `Write(data)` で PTY 出力を受け取り、`Lines()` で表示行を返す

**型**: `session.PTYDisplay`

### displayCache

PTYDisplay 内の atomic キャッシュ。エミュレータの `Write()` 完了後に毎回更新される `[]string`。`Lines()` はこのキャッシュをロックなしで読む。TUI の 60fps レンダリングが PTY 出力処理をブロックしない設計。

## プロセス管理

### ProcessSupervisor

PTY プロセスのライフサイクル (起動・停止・I/O・リサイズ) を管理するインフラ型。Session のドメインロジックからプロセス管理を分離するために抽出された。

**型**: `session.ProcessSupervisor`

### LaunchIntent / LaunchKind

セッション起動の意図を表す Value Object。`Manager.Launch()` がディスパッチする。

| Kind | 操作 |
|------|------|
| LaunchNew | 新規セッション作成 |
| LaunchResume | 既存セッション再開 (--resume) |
| LaunchFork | 既存セッションをフォーク (--resume --fork-session) |
| LaunchExternal | 外部ターミナルホスト (メタデータのみ管理) |

**型**: `session.LaunchIntent`, `session.LaunchKind`

## トークンとコスト

### TokenUsage

セッションのトークン消費量を追跡する Value Object。`EstimateCost(PricingPolicy)` で USD コストを自己計算する。

**型**: `session.TokenUsage`

### PricingPolicy

トークン単価を定義する Value Object。config.toml の `[pricing]` セクションから読み込まれる。TokenUsage がコスト計算時にこれを受け取る (インフラ非依存)。

**型**: `session.PricingPolicy`

## インフラ

### Hook

Claude Code のプラグインシステム。SessionStart, SessionEnd, Notification, Stop の4種のイベントを JSONL ファイルに書き出す。claude-deck はこれを監視して Status 遷移や SessionChain 更新を行う。

**関連**: `internal/hooks/`, `session.hookProcessor`

### hookProcessor

SessionEnd → SessionStart のペアリングを行うシングルスレッド状態機械。`/clear` 時に旧 ID → 新 ID の紐付けを確立する。ロック不要 (event watcher goroutine のみがアクセス)。

**型**: `session.hookProcessor`

### JSONL

Claude Code が `~/.claude/projects/<project>/<uuid>.jsonl` に書き出すセッションログ。対話のプロンプト、レスポンス、ツール実行、トークン使用量を含む。claude-deck の一次データソース。

**関連**: `internal/usage/`

### Store

claude-deck 固有のメタデータ永続化。`~/.local/share/claude-deck/sessions/<id>.json` に Session の Store 所有フィールドを書き出す。JSONL が「Claude Code の記録」、Store は「deck の記録」。

**関連**: `internal/store/`

### Workspace

jj (Jujutsu) のワークスペース機能で作成される隔離作業ディレクトリ。各セッションが独立したファイルシステム状態を持てる。

**関連**: `internal/jj/`
