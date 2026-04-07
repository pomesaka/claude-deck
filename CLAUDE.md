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
  pty/           PTY プロセス管理（claude CLI のラッパー）
  hooks/         Claude Code フックイベント連携
  usage/         JSONL パース・ストリーミング・トークン集計
  config/        TOML 設定ファイル
  store/         セッションメタデータ永続化（JSON）
  ghostty/       Ghostty ターミナルランチャー
  jj/            Jujutsu ワークスペース管理
  claudecode/    Claude Code パス解決・trust 設定
  debuglog/      デバッグログ
```

詳細なアーキテクチャは [docs/architecture.md](docs/architecture.md) を参照。

## 設計原則

**美しい設計はパターンの適用ではなく、ドメインの深い理解から生まれる。**

コードを書く前に、まずドメインを理解すること。Evans の "Refactoring Toward Deeper Insight" を常に意識する。

### 実装時の心得

1. **ドメインの言葉でコードを書く**: 変数名・関数名・型名はドメインエキスパートが読んで意味が通るものにする。技術用語でドメインの概念を隠さない
2. **責務を問う**: 新しい型や関数を作るとき「これは何を知っているか」「何をするか」「誰と協調するか」を自問する（RDD の思考法）
3. **境界を意識する**: Bounded Context の境界はどこか。この変更は境界を越えていないか。越えるなら明示的な変換層があるか
4. **不変条件を型で表現する**: 不正な状態を型レベルで表現不可能にする。ランタイムバリデーションより型制約を優先する
5. **集約は小さく保つ**: トランザクション整合性が本当に必要な範囲だけを集約にする。他の集約は ID で参照する
6. **ドメインイベントで疎結合にする**: モジュール間の直接的な依存より、ドメインイベントによる間接的な連携を検討する
7. **リファクタリングを恐れない**: 最初のモデルは必ず間違っている。ドメイン理解が深まったら躊躇なくモデルを作り直す

### レビュー時の観点（`/review-domain` で実行可能）

- ドメインの概念がコードに正確に反映されているか
- 責務の配置は適切か（God Object / Anemic Model になっていないか）
- Bounded Context の境界は明確か
- 依存の方向は正しいか（ドメインが外部に依存していないか）
- 命名はユビキタス言語に沿っているか

## 開発時の重要事項

### ロック順序（デッドロック防止）

Manager.mu → Session.mu の順で取得すること。逆順は ABBA デッドロックを起こす。
パターン: Manager.mu で候補リストをコピー → mu 解放 → Session.mu で個別アクセス。

### セッション ID の関係

| ID | 役割 |
|----|------|
| `Session.ID` | claude-deck 内部 ID（ランダム hex） |
| `ClaudeSessionID` | Claude Code が割り当てる UUID |
| `PreviousClaudeSessionID` | /clear 前の旧 Claude UUID（revert 用） |
| `CLAUDE_DECK_SESSION_ID` | 環境変数でフックに渡す deck ID |

`/clear` で ClaudeSessionID が変わるが、deck の Session.ID は不変。
ペアリングは `CLAUDE_DECK_SESSION_ID` 環境変数で行う。

### セッションステータス遷移

```
Idle ←→ Running ←→ WaitingApproval / WaitingAnswer
  ↓                        ↓
Completed / Error      (hook: Stop → Idle)
```

- Running: PTY 出力の Braille スピナー検知
- WaitingApproval/Answer: Hook Notification イベント
- Idle: Hook Stop イベント or スピナータイムアウト (3s)
- Completed: プロセス終了

### データソース優先度

- **JSONL** (Claude Code 一次データ): Prompt, TokenUsage, StartedAt, LastActivity
- **Store** (deck メタデータ): ID, Name, RepoPath, WorkspacePath, Status, PID
- **Runtime** (メモリのみ): LogLines, JSONLLogEntries, CurrentTool, emulator

### キーバインド

| キー | 操作 |
|------|------|
| `j/k` | カーソル移動 |
| `h/l` | ペイン切替 |
| `gg/G` | 先頭/末尾 |
| `Enter/i` | PTY 入力モード / 再開 |
| `Ctrl+D` | PTY 入力モード終了 |
| `n` | 新規セッション（Enter: ワークスペース付, C-Enter: 直接起動） |
| `r` | セッション再開 |
| `f` | セッションフォーク |
| `dd` | セッション削除（JSONL 含む完全削除） |
| `dD` | deck メタデータのみ削除（JSONL 残す、再発見される） |
| `x` | プロセス終了 |
| `t` | Ghostty ターミナル起動 |
| `/` | フィルタ |
| `tab` | 次の要手動介入セッションへジャンプ |

### ディレクトリ構成

```
~/.config/claude-deck/config.toml     設定
~/.local/share/claude-deck/
  sessions/                           セッション JSON メタデータ
  workspace/<encoded-repo>/<name>/    jj ワークスペース
  claude-deck-events.jsonl            フックイベントログ
  debug.log                           デバッグログ
~/.claude/projects/<project>/<uuid>.jsonl   Claude Code JSONL
```

### プラグインバージョン管理

プラグインのバージョンは以下の2箇所で管理しており、**常に同期させること**:

- `.claude-plugin/marketplace.json` の `"version"` — ユーザーに配布されるバージョン
- `internal/hooks/hooks.go` の `PluginVersion` 定数 — 起動時のバージョンチェックに使用

バージョンを上げるときは両方を同時に更新する。

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

- セッション数: 30（LRU で古いものを prune）
- LogLines: 1000 行/セッション
- スクロールバック: 2000 行
- JSONL LogEntries: 500 件
- ディスカバリ対象: 過去 14 日間
- メタデータ更新間隔: 5 秒
- スピナー Idle タイムアウト: 3 秒（固定）
- UI 更新レート: 60fps / 16ms debounce（固定）
