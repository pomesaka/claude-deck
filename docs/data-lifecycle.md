# 外部データのライフサイクル

claude-deck が読み書き・作成・削除する外部データの一覧と、各操作でいつ何が起きるかをまとめる。

## データ場所と所有者

| パス | 内容 | 所有者 |
|------|------|--------|
| `~/.local/share/claude-deck/deck.db` | セッションメタデータ（SQLite） | claude-deck |
| `~/.local/share/claude-deck/workspace/<encoded-repo>/<name>/` | jj ワークスペースディレクトリ | claude-deck |
| `~/.local/share/claude-deck/plugin/` | deck-status プラグイン（バイナリから書き出す） | claude-deck |
| `~/.local/share/claude-deck/debug.log` | デバッグログ（`CLAUDE_DECK_DEBUG=1` 時のみ） | claude-deck |
| `~/.claude/projects/<project>/<uuid>.jsonl` | 会話履歴・トークン使用量 | **Claude Code**（deck は読み取り専用） |

`deck.db` は TUI、CLI サブコマンド、hook コマンドが共有する。初回オープン時に旧形式の `sessions/*.json` があれば取り込み、`sessions.migrated-<timestamp>` に改名する。

`<encoded-repo>` はリポジトリの絶対パスを `-` で繋いだ文字列（例: `-Users-pomesaka-github.com-Accel-Hack-ADeT`）。

## 操作別ライフサイクル

### `n` — 新規セッション（ワークスペース付き）

**作成:**
```
deck.db                                                  # セッションの行を挿入
~/.local/share/claude-deck/workspace/<encoded>/<name>/
  ├─ .jj/                                               # jj workspace add
  ├─ .git → <repo>/.git                                 # symlink（colocated repos のみ）
  ├─ <workspace_symlinks の設定分>                       # config で指定した symlink
  └─ <tracked files>                                    # jj がリポジトリからコピー
~/.claude/projects/.../<uuid>.jsonl                      # Claude Code が起動時に作成
```

### `n` — 新規セッション（ワークスペースなし、`C-Enter`）

**作成:**
```
deck.db                                                  # セッションの行を挿入
~/.claude/projects/.../<uuid>.jsonl
```
ワークスペースディレクトリは作られない。`WorkspacePath` はリポジトリルートを指す。

### `r` — Resume

ワークスペースが存在する場合: ワークスペースをそのまま使い Claude を `--resume` で起動。

**ワークスペースが削除済み**（`x` で終了後）の場合:
```
# recreateWorkspace が走る
~/.local/share/claude-deck/workspace/<encoded>/<name>/   # 再作成
deck.db                                                  # 行の WorkspaceName/Path を更新
~/.claude/projects/.../<uuid>.jsonl                      # Claude Code が追記
```
ワークスペース再作成時の開始 revision は `LastJJRevision → LastJJParentRevision → trunk()` の優先順で使われる（ADR 009 参照）。

### `f` — Fork

**作成:**
```
deck.db                                                  # 新セッションの行を挿入
~/.local/share/claude-deck/workspace/<encoded>/<new-name>/
~/.claude/projects/.../<new-uuid>.jsonl                  # Claude Code が作成
```

### `x` — プロセス終了（Kill）

`claude-deck close` も同じ処理を実行する。

**削除:**
```
~/.local/share/claude-deck/workspace/<encoded>/<name>/   # os.RemoveAll で完全削除
```

**jj から登録解除:**
```
jj workspace forget <name>    # jj のワークスペース一覧から除去
```

**更新（persist）:**
```
deck.db の行
  ClosingAt = <時刻>               # 処理中に立て、完了時にクリア
  WorkspaceName = ""             # クリア
  WorkspacePath = ""             # クリア
  Status = Completed
  LastJJRevision = <change_id>   # @ の change_id（取得成功時のみ）
  LastJJParentRevision = <change_id>  # @- の change_id（取得成功時のみ）
```

`LastJJRevision` / `LastJJParentRevision` は `r` で resume するときに jj ワークスペースを再作成する際の開始 revision として使われる。取得に失敗した場合は両フィールドをクリア（空文字列）し、resume 時に `jj new trunk()` にフォールバックする。

**触らないもの:**
```
~/.claude/projects/.../<uuid>.jsonl    # Claude Code の所有物のため保持
```

## まとめ

```
         n (ws付き)     r (再開)       f (fork)       x (終了)
         ─────────────  ─────────────  ─────────────  ─────────────
session  作成           更新           作成           更新 (Status, ws)
workspace 作成          再作成*        作成           削除
JSONL    CC が作成      CC が追記      CC が作成      触らない

* ワークスペースが削除済みの場合のみ再作成
```

Claude Code JSONL は **claude-deck が所有しない**。会話を継続するために必要なデータであり、`x` では削除しない。手動で消したい場合は `~/.claude/projects/` を直接操作する。
