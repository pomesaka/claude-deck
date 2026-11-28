# 外部データのライフサイクル

claude-deck が読み書き・作成・削除する外部データの一覧と、各操作でいつ何が起きるかをまとめる。

## データ場所と所有者

`{DataDir}` は `config.toml` の `data_dir` で、環境変数 `CLAUDE_DECK_DATA_DIR` があればそちらを使う。既定は `$XDG_DATA_HOME/claude-deck`、`XDG_DATA_HOME` が無ければ `~/.local/share/claude-deck`。provider が `codex` なら既定の末尾に `-codex` が付く。

| パス | 内容 | 所有者 |
|------|------|--------|
| `{DataDir}/deck.db` | セッションメタデータ（SQLite） | claude-deck |
| `{DataDir}/workspace/<encoded-repo>/<name>/` | jj ワークスペースディレクトリ | claude-deck |
| `{DataDir}/plugin/` | deck-status プラグイン（バイナリから書き出す） | claude-deck |
| `{DataDir}/tui.lock` | 動いている TUI の pid と起動したパス。TUI が動いている間 flock を持ち、`claude-deck reload` はこれで TUI を見つける。消さない（ロックが外れていれば、残った中身は読まない） | TUI |
| `{DataDir}/rate-limits.json` | アカウントのレート制限（5 時間枠と 7 日枠）。セッションが報告するたびに上書きする | claude-deck |
| `~/.local/share/claude-deck/debug.log` | デバッグログ。`CLAUDE_DECK_DEBUG` を設定したときだけ書く。値が `1` か `true` ならこのパス（`XDG_DATA_HOME` があればその下。`data_dir` には従わない）、それ以外ならその値のパスに書く | claude-deck |
| `~/.claude/projects/<project>/<uuid>.jsonl` | 会話履歴 | **Claude Code**（deck は読み取り専用） |
| `~/.claude.json` | Claude Code の設定。`projects[<パス>].hasTrustDialogAccepted` が trust の登録 | **Claude Code**（deck はデータディレクトリと、作ったワークスペースの trust 登録だけを足す） |
| `~/.claude/settings.json` | 利用者の Claude Code の設定 | **利用者**（deck は書かない。以前の版が `statusLine` に入れたラッパーを、起動時に一度だけ元に戻す（`claudecode.RestoreStatusLine`）） |

`deck.db` は TUI、CLI サブコマンド、hook コマンドが共有する。store を開くときに旧形式の `{DataDir}/sessions/*.json` があれば、store が空のときだけ取り込み（外部セッションは除く）、ディレクトリを `sessions.migrated-<timestamp>` に改名する。

`<encoded-repo>` はリポジトリの絶対パスを `-` で繋いだ文字列（例: `-Users-pomesaka-github.com-Accel-Hack-ADeT`）。

## 隔離（isolation）

deck セッションは、行の `isolation` 列に、直接起動（`direct`）かワークスペース付き（`workspace`）かを持つ。作った後は変わらない。ワークスペースは常にセッション名（`name`）と同じ名前で `{DataDir}/workspace/<encoded>/<name>/` に作る。ワークスペース付きのセッションのワークスペースが今あるかは、`workspace_name` 列が空でないかで表す（close が空にする）。どの操作も、この 2 つを `session.Isolation` として読み書きする。

`isolation` 列を足す前の版の行は、store を開くときに一度だけ埋める。`workspace_name` があればワークスペース付き、`workspace_name` も `workspace_path` も空なら close でワークスペースを消したワークスペース付き、それ以外は直接起動とみなす（`store.LegacyIsolation`）。列を知らない古い版がその後に書いた行も、読むときに同じ規則で読む。

外部セッションを取り込むと、cwd が jj ワークスペースでも直接起動になる。claude-deck が作ったワークスペースではないので、close で消さない。

## 操作別ライフサイクル

### `n` — 新規セッション（ワークスペース付き）

**作成:**
```
deck.db                                                  # セッションの行を挿入
{DataDir}/workspace/<encoded>/<name>/
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
ワークスペースディレクトリは作られない。行の `isolation` は `direct` で、`workspace_path` は起動したディレクトリを指す。

### `r` — Resume

直接起動のセッションは、記録したディレクトリ（無ければリポジトリ）で Claude を `--resume` で起動する。ワークスペースは作らない。

ワークスペースが存在する場合: ワークスペースをそのまま使い Claude を `--resume` で起動。

**ワークスペースが削除済み**（`x` で終了後）の場合:
```
# workspaces.recreate が走る
{DataDir}/workspace/<encoded>/<name>/                    # 再作成
deck.db                                                  # 行の workspace_name / workspace_path を戻す
~/.claude/projects/.../<uuid>.jsonl                      # Claude Code が追記
```
ワークスペース再作成時の開始 revision は `LastJJRevision → LastJJParentRevision → trunk()` の優先順で使われる。

再作成に失敗したときは Claude を起動せず、セッションを Error にして理由を一覧に出す。本体リポジトリでは起動しない。作成の途中で失敗したワークスペースは `CreateWorkspaceAt` が消すので、原因を直してからもう一度 `r` を押せる。

### `f` — Fork

**作成:**
```
deck.db                                                  # 新セッションの行を挿入（forked_from に分岐元のランタイムセッション ID）
{DataDir}/workspace/<encoded>/<new-name>/
~/.claude/projects/.../<new-uuid>.jsonl                  # Claude Code が作成
```

### `x` — プロセス終了（Kill）

`claude-deck close` も同じ処理を実行する。

**削除:**
```
{DataDir}/workspace/<encoded>/<name>/                    # os.RemoveAll で完全削除
```

**jj から登録解除:**
```
jj workspace forget <name>    # jj のワークスペース一覧から除去
```

**更新（persist）:**
```
deck.db の行
  workspace_name = ""            # クリア（isolation は workspace のまま）
  workspace_path = ""            # クリア
  Status = Completed
  LastJJRevision = <change_id>   # @ の change_id（取得成功時のみ）
  LastJJParentRevision = <change_id>  # @- の change_id（取得成功時のみ）
```

`LastJJRevision` / `LastJJParentRevision` は `r` で resume するときに jj ワークスペースを再作成する際の開始 revision として使われる。`@` が空だと `jj workspace forget` で abandon されるので、`@-` も保存しておく。ワークスペースのディレクトリが既に無いときは両フィールドを空にし、resume 時に `jj new trunk()` にフォールバックする。

`@` と `@-` を読む `jj log` は、編集途中のファイルを `@` に取り込む（snapshot）。これが失敗したときは、ワークスペースを削除も forget もせず、行の `workspace_name` と `workspace_path` を残す。取り込めていない変更は、ディレクトリを消すと取り戻せないためだ。セッションは Completed になり、`r` はそのワークスペースをそのまま使う。TUI はステータス行に残したことを、`claude-deck close` は stderr に残したパスを出す。prune も同じ規則で消す（下の prune の節）。

**触らないもの:**
```
~/.claude/projects/.../<uuid>.jsonl    # Claude Code の所有物のため保持
~/.claude.json の projects[<ワークスペース>]   # r で同じパスに作り直すので残す
```

### prune — 上限を超えた終了済みセッションの削除（自動）

利用者の操作ではない。store のセッションが `max_sessions`（既定 30）を超えると、終了済みのうち古いものから消す。TUI の起動時と、セッションを作った直後に動く。消されたセッションは一覧から消え、再開できない。動いているセッションと close 中のセッションは消さない。ピンしたセッションも、終了済みで古ければ消す。

**削除:**
```
deck.db の行
{DataDir}/workspace/<encoded>/<name>/                    # x を押さずに終えたセッションの分。jj workspace forget も行う
~/.claude.json の projects[<ワークスペースとその配下>]      # Claude のみ
```

- ワークスペースを消す前に、編集途中のファイルを jj の snapshot で `@` に取り込む。snapshot に失敗したときは、ワークスペースを消さずに残す
- 同じ名前のワークスペースを持つ行が残っているときは、何も消さない
- 直接起動のセッションでは、ワークスペースも登録も消さない。同じ名前のワークスペースのディレクトリがあっても、その行のものではない

**触らないもの:**
```
~/.claude/projects/.../<uuid>.jsonl
```

### `claude-deck gc` — 持ち主のいないデータの掃除

prune が消しきれなかったものを片付ける。prune の途中でプロセスが止まったときや、snapshot に失敗してワークスペースを残したときに、行の無いワークスペースができる。`--dry-run` を付けると、消さずに対象だけを返す。

**削除:**
```
{DataDir}/workspace/<encoded>/<name>/                    # store に同じリポジトリ・同じ名前の行が無いもの
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
