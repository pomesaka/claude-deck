# claude-deck

Claude Code セッションを一括管理する TUI ダッシュボード。

## セットアップ

### 必須環境

- Go 1.26+
- `encoding/json/v2` を使用 → **`GOEXPERIMENT=jsonv2` が必須**

### ビルド・テスト・実行

```bash
cd claude-deck
GOEXPERIMENT=jsonv2 go build -o claude-deck ./cmd/claude-deck
GOEXPERIMENT=jsonv2 go test ./...
./claude-deck
```

## アーキテクチャ概要

```
cmd/claude-deck/main.go   エントリポイント
internal/
  session/       セッションライフサイクル管理（Manager が中心）
  tui/           Bubble Tea TUI（Model, View, Keys）
  usage/         JSONL パース・ストリーミング・トークン集計（Claude / Codex の違いは format の 2 実装）
  config/        TOML 設定ファイル
  store/         セッションメタデータ永続化（SQLite）
  ghostty/       Ghostty ターミナルランチャー
  jj/            Jujutsu ワークスペース管理
  claudecode/    Claude Code パス解決・trust 設定
  debuglog/      デバッグログ
deckmod/         deck-status プラグイン（バイナリに埋め込み）
```

詳細なアーキテクチャは [docs/architecture.md](docs/architecture.md) を参照。

## 設計原則

[docs/design-principles.md](docs/design-principles.md) を参照。実装・レビュー時に常に意識すること。

`/review-domain` コマンドでドメイン分析・アーキテクチャレビューを実行可能。

## ドキュメント

新規参加者は以下の順で読むこと:

1. [docs/00-glossary.md](docs/00-glossary.md) — ドメイン用語集
2. [docs/01-overview.md](docs/01-overview.md) — システム全体像とデータフロー
3. この CLAUDE.md — 開発規約
4. [docs/architecture.md](docs/architecture.md) — パッケージ構成と初期化フロー
5. [docs/concurrency.md](docs/concurrency.md) — 並行処理ルール
6. [docs/data-lifecycle.md](docs/data-lifecycle.md) — 外部データ（workspace・store・JSONL）のライフサイクル
7. [docs/adr/](docs/adr/) — 設計判断の記録

ドメイン概念を追加・変更したらコードと同時にドキュメントも更新すること。

### ADR の追加ルール

以下のような設計判断が生じたときは **積極的に** `docs/adr/NNN-*.md` を作成すること:

- 「なぜこの実装を選んだか」が後から自明でない場合
- 複数の代替案を比較・却下した場合
- バグ修正の結果として設計の前提が変わった場合
- 既存の ADR が覆された・拡張された場合

フォーマット: ステータス / コンテキスト / 決定 / 結果（良い点・悪い点・却下した代替案）。
番号は既存の最大値 + 1。コードレビューと同じ PR に含めること。

## 開発時の重要事項

### ロック順序（デッドロック防止）

Manager.mu → Session.mu の順で取得すること。逆順は ABBA デッドロックを起こす。
パターン: Manager.mu で候補リストをコピー → mu 解放 → Session.mu で個別アクセス。
詳細は [docs/concurrency.md](docs/concurrency.md) を参照。

### セッション ID の関係

| ID | 役割 |
|----|------|
| `Session.ID` (`DeckSessionID`) | claude-deck 内部 ID（ランダム hex） |
| `ClaudeSessionID` | Claude Code が割り当てる UUID |
| `SessionChain` | /clear を跨いだ ClaudeSessionID の履歴（古い順） |
| `ForkedFrom` | フォークの分岐元の ClaudeSessionID（別セッションの SessionChain の要素） |
| `CLAUDE_DECK_SESSION_ID` | 環境変数で各セッションに渡す deck ID |

`/clear` で ClaudeSessionID が変わるが、deck の Session.ID は不変。
hook は環境変数の deck ID を使って store の行を特定するので、ClaudeSessionID との突き合わせは要らない。

起動する Claude Code には次の環境変数を渡す。

| 環境変数 | 内容 |
|----------|------|
| `CLAUDE_DECK_SESSION_ID` | deck ID |
| `CLAUDE_DECK_DATA_DIR` | データディレクトリ（`config.Load` が `data_dir` の上書きとして読む） |
| `CLAUDE_DECK_BIN` | claude-deck バイナリの絶対パス |

### セッションステータス遷移

```
Idle ←→ Running ←→ WaitingApproval / WaitingAnswer
  ↓                        ↓
Completed / Error      (hook: turn.complete → Idle)
```

- Running/WaitingApproval/Answer/Idle: deck-status プラグインが `claude-deck hook status` で store に書く（[docs/hooks.md](docs/hooks.md)）
- Completed: ウィンドウのコマンド末尾の `claude-deck hook exited`、`x` / `claude-deck close`、ウィンドウ消失の検知のいずれか
- 遷移の規則は `internal/session/transitions.go` の純関数。どのプロセスも store のトランザクション内で適用する。`Status` を書くのはこのファイルだけ（[ADR-015](docs/adr/015-cleanup-after-store-migration.md)）

### データソース優先度（→ [用語集: Projection](docs/00-glossary.md#projection-投影)）

- **JSONL** (Claude Code 一次データ): Prompt, TokenUsage, StartedAt, LastActivity
- **Hook** (リアルタイム通知): Status 遷移, SessionChain 更新。`claude-deck hook` が store に書く
- **Store** (SQLite `deck.db`, 信頼できる唯一の情報源): ID, Name, RepoPath, WorkspacePath, Status, PID, SessionChain, ForkedFrom, ClosingAt
- **Runtime** (メモリのみ): CurrentTool

`Manager.sessions` は store を `Manager.Reload` で読み直した投影。TUI は `PRAGMA data_version` を 200ms ごとに見て、他プロセス（CLI・hook）の書き込みを検知する。JSONL から発見した外部セッションは store に入れずメモリだけに持つ。
store が書く項目（Status, SessionChain, PID, ワークスペース等）は常に store に従い、JSONL・jj から TUI が投影する項目（Prompt, TokenUsage, BookmarkName 等）は、セッションが初めてメモリに現れるときだけ store から読む。

### キーバインド

| キー | 操作 |
|------|------|
| `j/k` | カーソル移動 |
| `gg/G` | 先頭/末尾 |
| `Enter` | tmux ウィンドウにフォーカス / 再開 |
| `n` | 新規セッション（Enter: ワークスペース付, C-Enter: 直接起動） |
| `r` | セッション再開 |
| `f` | セッションフォーク |
| `x` | プロセス終了 + ワークスペース削除（JSONL・メタデータは保持） |
| `t` | Ghostty ターミナル起動 |
| `R` | 再描画 |
| `/` | フィルタ |
| `tab` | 次の要手動介入セッションへジャンプ |
| `C-c` / `C-z` | 終了（確認あり / 確認なし） |

`f` と `t` だけ `config.toml` の `[keybinds]`（`fork`、`open_term`）で変えられる。

### CLI サブコマンド

store と tmux を直接操作するので、TUI が起動していなくても実行できる。出力は JSON（`tree` だけテキスト）。詳細は [ADR-011](docs/adr/011-store-as-source-of-truth.md)。

| コマンド | 対応するキー |
|------|------|
| `claude-deck new [--dir DIR] [--no-workspace]` | `n`（`--no-workspace` は C-Enter） |
| `claude-deck list` | 一覧表示。`session_chain` と `forked_from` で `/clear` とフォークの系譜も返す |
| `claude-deck tree` | なし。Claude Code のセッションを `/clear` とフォークの親子関係でたどった木を、テキストで出す（[ADR-012](docs/adr/012-fork-lineage.md)） |
| `claude-deck close <ID\|NAME>` | `x` |
| `claude-deck gc [--dry-run]` | なし。どのセッションのものでもないワークスペースと、消えたワークスペースについての `~/.claude.json` の登録を消す |

内部用に `claude-deck hook status|session-start|exited|rate-limits --session <ID>` がある。deck-status プラグインとウィンドウのコマンドが呼ぶもので、手で実行するものではない。

`x` と `close` は store の `closing_at` で複数プロセスの同時 close を防ぐ（2 分でタイムアウト）。

### ディレクトリ構成

```
~/.config/claude-deck/config.toml     設定
~/.local/share/claude-deck/
  deck.db                             セッションメタデータ（SQLite）
  plugin/                             deck-status プラグイン（起動時にバイナリから書き出す）
  rate-limits.json                    アカウントのレート制限（セッションが報告する）
  workspace/<encoded-repo>/<name>/    jj ワークスペース
  debug.log                           デバッグログ
~/.claude/projects/<project>/<uuid>.jsonl   Claude Code JSONL
~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl Codex JSONL
```

### Runtime provider

`config.toml` の `[runtime] provider` で `claude`（デフォルト）または `codex` を選択する。

```toml
[runtime]
provider = "codex"

[commands]
codex = "codex"
```

- `claude`: Claude Code plugin hooks を status / SessionChain 更新に使う。transcript は `~/.claude/projects`。
- `codex`: Codex JSONL (`~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`) を transcript と runtime activity の一次データとして使う。

### deck-status プラグイン

`deckmod/` の Claude Code プラグイン（Mods の function hook）が、セッションのステータスと `/clear` を `claude-deck hook` 経由で store に書く。アカウントのレート制限も同じ経路で受け取り、`{DataDir}/rate-limits.json` に書く（[ADR-013](docs/adr/013-rate-limits-from-plugin.md)）。

- バイナリに埋め込まれ、claude-deck の起動時に `{DataDir}/plugin/` へ書き出される（内容が同じファイルは書き換えない）
- claude-deck が起動する全セッションに `--plugin-dir {DataDir}/plugin` を渡す。ユーザーがプラグインを別途インストールする必要はない
- イベントとステータスの対応は [docs/hooks.md](docs/hooks.md)
- セッション開始時に、claude-deck の CLI が使えることをモデルに伝える。使い方は同梱のスキル `deck-status:claude-deck`（`deckmod/skills/claude-deck/SKILL.md`）にあるので、CLI を変えたらスキルも直す

### プロジェクト検出（モノレポ対応）

`config.toml` の `[discovery]` セクションでマーカーファイルを指定すると、jj リポジトリ内のサブプロジェクトも候補に表示される。

```toml
[discovery]
project_markers = ["go.mod", "package.json", "Cargo.toml"]
excludes = ["Library", ".cache", "node_modules", ".git"]
```

- `project_markers` が空（デフォルト）の場合、リポジトリルートのみが候補
- `project_markers` を設定すると、各リポジトリ内でマーカーファイルを `fd` で検索し、見つかったディレクトリも候補に追加
- リポジトリルートは常に候補に含まれる
- 別の `.jj` を持つ入れ子のリポジトリの中は、外側のサブプロジェクトにしない（入れ子のリポジトリは自分の候補として出る）

関連ファイル: `config.go` (`DiscoveryConfig`), `wizard.go` (`discoverRepos`, `findProjectDirs`)

### ワークスペース symlink 設定

`jj workspace add` で作成されるワークスペースには `.env` 等の untracked ファイルがコピーされない。
`config.toml` の `[projects]` セクションでプロジェクトごとに symlink 対象を指定できる。

```toml
[projects."/Users/foo/myrepo"]
workspace_symlinks = [".env", ".env.local", "secrets/"]
```

- パスはリポジトリルートからの相対パス
- ソースが存在しなければスキップ（`.env` が無いリポジトリでも安全）
- 宛先が既に存在すればスキップ（jj が tracked ファイルを作成済みの場合）
- 絶対パスや `..` を含むパスはセキュリティのためスキップ

関連ファイル: `config.go` (`ProjectConfig`, `WorkspaceSymlinks()`), `jj.go` (`createExtraSymlink`)

### Claude Code への追加ディレクトリ設定

`config.toml` の `[projects]` セクションで `add_dirs` を指定すると、セッション起動時に `--add-dir` フラグとして渡される。

```toml
[projects."/Users/foo/myrepo"]
add_dirs = ["../shared-lib", "/absolute/path/to/docs"]
```

- 相対パスはリポジトリルートからの相対パスとして解決
- 絶対パスはそのまま使用
- Create / Resume / Fork の全セッション起動パターンで適用

関連ファイル: `config.go` (`ProjectConfig`, `ResolvedAddDirs()`), `session/manager.go` (`buildAddDirArgs`)

### 上限値（デフォルト値、config.toml の `[session]` で変更可）

- セッション数: 30（超えると終了済みの古いものを prune。残っているワークスペースと `~/.claude.json` の登録も消す。[docs/data-lifecycle.md](docs/data-lifecycle.md)）
- JSONL LogEntries: 500 件
- ディスカバリ対象: 過去 14 日間
- メタデータ更新間隔: 5 秒
- UI 更新レート: 60fps / 16ms debounce（固定）
