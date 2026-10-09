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
| `~/.claude.json` | Claude Code の設定。`projects[<パス>].hasTrustDialogAccepted` が trust の登録 | **Claude Code**（deck はデータディレクトリと、作ったワークスペースの trust 登録だけを足す） |

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
~/.claude.json                                           # projects[<ワークスペース>] を trust 済みにする（Claude のみ）
~/.claude/projects/.../<uuid>.jsonl                      # Claude Code が起動時に作成
```

trust の登録はワークスペースごとに行う。Claude Code は cwd から git ルートまでしか登録を探さず、`.git` の symlink を持つワークスペースは自分が git ルートになるので、データディレクトリの登録では trust ダイアログを止められない（Claude Code 2.1.295 で確認）。登録はワークスペースを消しても残る。

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
deck.db                                                  # 新セッションの行を挿入（forked_from に分岐元の ClaudeSessionID）
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
~/.claude.json の projects[<ワークスペース>]   # r で同じパスに作り直すので残す
```

### prune — 上限を超えた終了済みセッションの削除（自動）

利用者の操作ではない。store のセッションが `max_sessions`（既定 30）を超えると、終了済みのうち古いものから消す。TUI の起動時と、セッションを作った直後に動く。消されたセッションは一覧から消え、再開できない。動いているセッションと close 中のセッションは消さない。

**削除:**
```
deck.db の行
~/.local/share/claude-deck/workspace/<encoded>/<name>/   # x を押さずに終えたセッションの分。jj workspace forget も行う
~/.claude.json の projects[<ワークスペースとその配下>]      # Claude のみ
```

- ワークスペースを消す前に、編集途中のファイルを jj の snapshot で `@` に取り込む。snapshot に失敗したときは、ワークスペースを消さずに残す
- 同じ名前のワークスペースを持つ行が残っているときは、何も消さない
- ワークスペースなしで起動したセッションでは、リポジトリ本体の登録に触れない

**触らないもの:**
```
~/.claude/projects/.../<uuid>.jsonl
```

### `claude-deck gc` — 持ち主のいないデータの掃除

prune が消しきれなかったものを片付ける。prune の途中でプロセスが止まったときや、snapshot に失敗してワークスペースを残したときに、行の無いワークスペースができる。`--dry-run` を付けると、消さずに対象だけを返す。

**削除:**
```
~/.local/share/claude-deck/workspace/<encoded>/<name>/   # store に同じリポジトリ・同じ名前の行が無いもの
~/.claude.json の projects[<ワークスペースとその配下>]      # 上で消したものと、ディレクトリが既に無いもの（Claude のみ）
```

- 終了済みの行でも、名前が一致すれば持ち主がいるとみなす。close したセッションは同じパスに作り直されるため
- 作られてから 1 時間たっていないディレクトリは消さない。セッションの作成は、ワークスペースを作ってから store に行を入れるので、その間のワークスペースには行が無い
- jj のワークスペースなら、snapshot を試してから `jj workspace forget` する。prune と違い、snapshot に失敗しても消す。失敗するワークスペースを片付ける手段がほかに無いため
- `.jj` の無いディレクトリは、jj を呼ばずにそのまま消す

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
