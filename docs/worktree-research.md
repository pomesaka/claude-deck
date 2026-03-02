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

## git worktree への移行検討

### メリット

1. **jj 不要** — git さえあれば動作する。ユーザーの導入ハードルが大幅に下がる
2. **Claude Code との親和性** — Claude Code 自体が git worktree を前提に設計されている
3. **標準的なワークフロー** — git ブランチベースの差分管理は一般的で馴染みやすい

### 実装方針

#### 1. WorkspaceProvider インターフェースを定義

```go
// internal/workspace/provider.go
type Provider interface {
    Create(repoPath, name, wsPath string) error
    Remove(repoPath, name string) error
    List(repoPath string) ([]string, error)
    Status(wsPath string) (string, error)
}
```

#### 2. git worktree 実装を追加

```go
// internal/workspace/git.go
type GitProvider struct{}

func (g *GitProvider) Create(repoPath, name, wsPath string) error {
    // git worktree add -b worktree-<name> <wsPath>
}

func (g *GitProvider) Remove(repoPath, name string) error {
    // git worktree remove <path>
}
```

#### 3. jj 実装をラップ

```go
// internal/workspace/jj.go
type JJProvider struct{}

// 既存の internal/jj/jj.go の関数をラップ
```

#### 4. Config で切り替え

```toml
[commands]
workspace_backend = "git"  # or "jj" (default: "git")
```

### 変更が必要なファイル

| ファイル | 変更内容 |
|----------|----------|
| 新規 `internal/workspace/` | Provider インターフェース + git/jj 実装 |
| `internal/session/manager.go` | Provider を注入して使用 |
| `internal/config/config.go` | `workspace_backend` 設定追加 |
| `cmd/claude-deck/main.go` | 設定に基づいて Provider を選択 |

### 注意点

- git worktree は **同一ブランチを複数 worktree でチェックアウトできない** 制約がある
- worktree 内の `.claude/projects/` パス解決が変わる可能性がある
- jj 利用者向けに後方互換を維持する（デフォルトを git にし、config で jj も選べるようにする）
- worktree の配置先を `<repo>/.claude/worktrees/<name>/` にするか `~/.local/share/claude-deck/workspace/` に置くかの検討が必要
  - Claude Code と同じ `<repo>/.claude/worktrees/` に合わせると自然だが、`.gitignore` 設定が必要
  - 現在のように `~/.local/share/` に置けばリポジトリを汚さない
