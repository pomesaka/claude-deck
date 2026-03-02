# Claude Code Worktree 機能調査

## Claude Code の Worktree 機能

### 概要

Claude Code は `--worktree` フラグ（短縮形 `-w`）で **git worktree** を自動作成し、同一リポジトリで複数の独立した作業ディレクトリを持てる機能を提供している。

- **v2.1.49** (2026-02-19): CLI worktree 対応
- **v2.1.50** (2026-02-20): `WorktreeCreate` / `WorktreeRemove` フックイベント追加
- Desktop アプリ、IDE 拡張、Web、モバイルでも利用可能

### CLI での使い方

```bash
# 自動生成名で worktree 作成
claude --worktree

# 名前を指定して作成
claude --worktree feature-auth
claude -w bugfix-123
```

### 仕組み

| 項目 | 詳細 |
|------|------|
| **配置場所** | `<repo>/.claude/worktrees/<name>/` |
| **ブランチ命名** | `worktree-<name>` |
| **ベースブランチ** | デフォルトのリモートブランチから作成 |
| **共有される情報** | git 履歴、リモート、設定 |
| **分離される情報** | ファイル内容、`.claude/` ディレクトリ |

### 自動クリーンアップ

| シナリオ | 挙動 |
|----------|------|
| 変更なし | worktree とブランチを自動削除 |
| 変更あり | keep / remove をプロンプトで確認 |

### Agent SDK での利用

サブエージェントに `isolation: worktree` を設定すると、各サブエージェントが独立した worktree で作業できる。

```markdown
# .claude/agents/worker.md
---
name: parallel-worker
isolation: worktree
---
```

### Hooks

| Hook | 用途 | 備考 |
|------|------|------|
| `WorktreeCreate` | worktree 作成時のカスタム処理（非 git VCS 対応等） | stdout に worktree パスを出力する必要あり。`command` タイプのみ |
| `WorktreeRemove` | worktree 削除時のクリーンアップ | 入力 JSON に `worktree_path` を含む。`command` タイプのみ |

設定例（`.claude/settings.json`）:

```json
{
  "hooks": {
    "WorktreeCreate": [{
      "hooks": [{
        "type": "command",
        "command": "bash \"$CLAUDE_PROJECT_DIR\"/.claude/hooks/worktree.sh",
        "timeout": 30
      }]
    }],
    "WorktreeRemove": [{
      "hooks": [{
        "type": "command",
        "command": "bash \"$CLAUDE_PROJECT_DIR\"/.claude/hooks/worktree-cleanup.sh",
        "timeout": 15
      }]
    }]
  }
}
```

### 既知の制限

- **`ExitWorktree` ツールがない** — worktree に入った後、同一セッション内で抜けられない（[#29436](https://github.com/anthropics/claude-code/issues/29436)）
- 各 worktree で依存関係のセットアップ（`npm install` 等）が別途必要
- リソース消費: 複数 worktree の並行実行は CPU/メモリ/API コストが大きい

### 関連ツール

- [claude-worktree-hooks](https://github.com/tfriedel/claude-worktree-hooks) — worktree 自動セットアップ（env、依存関係、ポート）
- [parallel-code](https://github.com/johannesjo/parallel-code) — Claude Code / Codex / Gemini を worktree で並行実行
- [agenttools/worktree](https://github.com/agenttools/worktree) — GitHub Issues + Claude Code 連携の worktree 管理 CLI

## claude-deck の現在のアーキテクチャ

### 現在: Jujutsu (jj) ワークスペース

claude-deck は現在 Jujutsu のワークスペース機能で分離を実現している。

```
~/.local/share/claude-deck/workspace/<encoded-repo>/<ws-name>/
```

`internal/jj/jj.go` のインターフェースは 4 関数のみ:

- `CreateWorkspaceAt(repoPath, name, wsPath)` — ワークスペース作成
- `ForgetWorkspace(repoPath, name)` — ワークスペース削除
- `ListWorkspaces(repoPath)` — ワークスペース一覧
- `WorkspaceStatus(wsPath)` — ステータス取得

セッション起動フロー:

```
Manager.CreateSession()
  → jj.CreateWorkspaceAt(repoPath, wsName, wsPath)
  → pty.Start(WorkDir: wsPath)
```

Claude Code は常にワークスペースディレクトリ内で起動される。

## jj と git worktree の互換性

### 結論: 併用不可

jj と git worktree は現状 **二者択一** である。

| 組み合わせ | 結果 |
|------------|------|
| git worktree 内で `jj git init --colocate` | **v0.38.0 で明示的に拒否** |
| jj colocated repo で `git worktree add` | detached HEAD のため `-b` 必須、かつ不安定 |
| `jj workspace add` のセカンダリ | `.git` を持たない → git 依存ツールが動かない |
| jj colocated repo で `claude --worktree` | detached HEAD で静かに失敗（[#27466](https://github.com/anthropics/claude-code/issues/27466)） |

### 経緯

| 時期 | 状況 |
|------|------|
| v0.35.0 (2025-11) | `jj git colocation enable/disable/status` コマンド追加 |
| PR #4588 | colocated workspaces using git worktrees — 分割されてクローズ |
| PR #4644 | 基盤部分（minimal）— マージされずレビュー停滞中 |
| PR #4678 | `jj workspace add --colocate` — gix/git2 の worktree 操作制限で未完 |
| v0.38.0 (2026-02) | `jj git init --colocate` が git worktree 内で拒否されるように |

### 技術的な理由

- git worktree は `.git` **ディレクトリ** ではなく `.git` **ファイル**（gitfile）を使う。jj の colocation ロジックは `.git` ディレクトリを前提としている
- jj colocated repo は常に **detached HEAD** になる。git worktree の作成に支障がある
- `jj workspace add` のセカンダリワークスペースは pure jj であり、`.git` を持たない

### 参考リンク

- [Git compatibility - Jujutsu docs](https://docs.jj-vcs.dev/latest/git-compatibility/) — `git-worktree: No`
- [PR #4588](https://github.com/martinvonz/jj/pull/4588) — Colocated workspaces using git worktrees（クローズ）
- [PR #4644](https://github.com/martinvonz/jj/pull/4644) — Colocated workspaces, minimal（停滞中）
- [Discussion #6286](https://github.com/jj-vcs/jj/discussions/6286) — セカンダリワークスペースに `.git` が欲しいという要望
- [Claude Code #27466](https://github.com/anthropics/claude-code/issues/27466) — `--worktree` が jj colocated repo で失敗

## 実装方針: Claude Code `--worktree` への委譲

### 基本戦略

ワークスペース作成を claude-deck 側で行わず、Claude Code の `--worktree` に委譲する。

- **git リポジトリ** → `claude --worktree <name>` でビルトイン git worktree を使用
- **jj リポジトリ** → `claude --worktree <name>` + `WorktreeCreate`/`WorktreeRemove` フックで jj workspace を使用

### セッション起動フローの変更

```
# 現在
Manager.CreateSession()
  → jj.CreateWorkspaceAt(repoPath, wsName, wsPath)  # claude-deck が管理
  → pty.Start(WorkDir: wsPath, args: ["--agent", name])

# 新規
Manager.CreateSession()
  → pty.Start(WorkDir: repoPath, args: ["--worktree", name, "--agent", name])
  # Claude Code が worktree を作成し、その中で起動
```

### jj 統合: WorktreeCreate/WorktreeRemove フック

Claude Code の `WorktreeCreate` フックはビルトイン git worktree 動作を **置き換える**。
jj リポジトリではこのフックで `jj workspace add` を呼ぶ。

#### フックスクリプト案

```bash
#!/bin/bash
# claude-deck-worktree-create.sh
INPUT=$(cat)
NAME=$(echo "$INPUT" | jq -r .name)
CWD=$(echo "$INPUT" | jq -r .cwd)

if [ -d "$CWD/.jj" ]; then
    # jj リポジトリ: jj workspace で分離
    DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/claude-deck"
    ENCODED=$(echo "$CWD" | sed 's|^/|-|; s|/|-|g')
    WS_PATH="$DATA_DIR/workspace/$ENCODED/$NAME"
    mkdir -p "$(dirname "$WS_PATH")"
    jj workspace add --name "$NAME" "$WS_PATH" --repository "$CWD"
    echo "$WS_PATH"
else
    # git リポジトリ: git worktree で分離
    WS_PATH="$CWD/.claude/worktrees/$NAME"
    mkdir -p "$(dirname "$WS_PATH")"
    git -C "$CWD" worktree add -b "worktree-$NAME" "$WS_PATH"
    echo "$WS_PATH"
fi
```

```bash
#!/bin/bash
# claude-deck-worktree-remove.sh
INPUT=$(cat)
WS_PATH=$(echo "$INPUT" | jq -r .worktree_path)

if [ -z "$WS_PATH" ] || [ "$WS_PATH" = "null" ]; then
    exit 0
fi

# jj workspace かどうかは親リポジトリの .jj で判定
# WorktreeRemove は worktree_path しか受け取れないため、
# パスから逆引きする必要がある
if echo "$WS_PATH" | grep -q "/.local/share/claude-deck/workspace/"; then
    # claude-deck 管理パス → jj workspace
    WS_NAME=$(basename "$WS_PATH")
    # jj workspace forget は repoPath が必要 → パスからデコード
    # （実装時に要検討）
    :
else
    # .claude/worktrees/ 配下 → git worktree
    REPO_PATH=$(echo "$WS_PATH" | sed 's|/.claude/worktrees/.*||')
    git -C "$REPO_PATH" worktree remove "$WS_PATH" 2>/dev/null || true
fi
```

### フックのインストール方法

#### 方針 A: プラグインに常時インストール（推奨）

`plugin/hooks/hooks.json` に `WorktreeCreate`/`WorktreeRemove` を追加。
スクリプト内で `.jj/` の有無を判定して分岐する。

**メリット**: シンプル。ユーザーの追加設定不要。
**デメリット**: git リポジトリでも Claude Code のビルトイン動作を上書きする。将来の CC 機能拡張に追従しない可能性。

#### 方針 B: jj リポジトリのみプロジェクトレベルで動的インストール

claude-deck が jj リポジトリを検知した場合のみ、`.claude/settings.local.json` に
WorktreeCreate/WorktreeRemove フックを書き出す。git リポジトリではビルトインを維持。

**メリット**: git では Claude Code のネイティブ体験を維持。
**デメリット**: `.claude/settings.local.json` を動的に管理する複雑さ。

### Config

```toml
[commands]
workspace_backend = "auto"  # "auto" | "git" | "jj"
# auto: .jj/ があれば jj, なければ git
# git:  常に git worktree（Claude Code --worktree ビルトイン）
# jj:   常に jj workspace（WorktreeCreate フック経由）
```

### Manager の変更

```go
// Manager.CreateSession() の変更点
func (m *Manager) CreateSession(ctx context.Context, repoPath string, cols, rows int) (*Session, error) {
    sess := NewSession(repoPath, repoName)

    // ワークスペース作成は Claude Code に委譲
    // --worktree <name> で Claude Code が作成・CWD 変更を行う
    args := []string{"--worktree", sess.Name, "--agent", sess.Name}

    proc, err := pty.Start(ctx, pty.StartOptions{
        WorkDir:        repoPath,  // リポジトリルートから起動
        AdditionalArgs: args,
        Env:            []string{"CLAUDE_DECK_SESSION_ID=" + sess.ID},
        // ...
    })
    // ...

    // WorkspacePath は Claude Code が作成した先のパス
    // git: <repo>/.claude/worktrees/<name>/
    // jj:  ~/.local/share/claude-deck/workspace/<encoded>/<name>/
    sess.WorkspacePath = computeWorktreePath(repoPath, sess.Name, backend)
}
```

### DeleteSession の変更

```go
// 現在: jj.ForgetWorkspace() を直接呼ぶ
// 新規: バックエンドに応じたクリーンアップ
func (m *Manager) DeleteSession(sessionID string) (string, error) {
    // ...
    switch m.config.WorkspaceBackend {
    case "jj":
        jj.ForgetWorkspace(repoPath, wsName)
        os.RemoveAll(wsPath)
    case "git":
        // git worktree remove + branch delete
        exec.Command("git", "-C", repoPath, "worktree", "remove", wsPath).Run()
        exec.Command("git", "-C", repoPath, "branch", "-D", "worktree-"+wsName).Run()
    }
}
```

### 未解決事項

- `--worktree` と `--agent` フラグの併用が可能か要検証
- `--worktree` と `--resume` の組み合わせ — resume 時は worktree が既存なので `--worktree` 不要?
- プラグインのフックスクリプトの配布方法（バイナリに埋め込み? 外部ファイル?）
- git worktree の `<repo>/.claude/worktrees/` が `.gitignore` に入っているか（Claude Code が管理?）
