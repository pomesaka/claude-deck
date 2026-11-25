# ドメイン用語集

claude-deck のコードと会話で使われる用語の定義。コードを読む前にここで概念を掴む。

> 用語の追加・変更時はこのファイルとコードを同時に更新すること。
> 対応する型がある場合は `パッケージ.型名` で示す。

## セッション

### Session

Claude Code との1つの対話セッション。claude-deck の中心概念。

Claude Code のプロセスライフサイクル、対話履歴、作業ディレクトリを追跡する。deck が割り振る ID（`session.DeckSessionID`、ランダムな hex）は Session の一生を通じて変わらない。1 つの Session は、`/clear` のたびに変わる複数のランタイムセッション ID を持つ（→ SessionChain）。

**型**: `session.Session`

### セッション名

deck が作成時に付ける名前（`emiri-f7d9` のような形）。作った後は変わらない。jj ワークスペースの名前とディレクトリ名、Claude Code の `--name`（SendMessage の宛先）を兼ねる。表示だけを変えたいときは Alias を使う。外部セッションでは、cwd のワークスペースのディレクトリ名か、ランタイムセッション ID の先頭 8 文字になる。

**フィールド**: `Session.Name`

### サブプロジェクト

リポジトリの中の、`project_markers`（`go.mod` など）のあるディレクトリ。`n` でサブプロジェクトを選ぶと、そのディレクトリで Claude Code を起動する。ワークスペース付きなら、ワークスペースの中の同じ相対パスで起動する。

**フィールド**: `Session.SubProjectDir`（リポジトリのルートからの相対パス。ルートなら空）

### Alias

利用者かセッション自身が `claude-deck alias` で付ける表示用の名前。TUI の一覧では、セッション名と並べて 2 行目の右に出る（preview ウィンドウのタイトルでは、セッション名の代わりに出る）。セッション名は jj ワークスペースの名前とディレクトリ名、Claude Code のセッション名（SendMessage の宛先）を兼ねていて変えられないので、表示だけを別に持つ。使えるのは英数字と `-` `_` `.` で、一意である必要はない。`close` などで操作するセッションの指定には使えない（`list --alias` で探せる）。

**フィールド**: `Session.Alias`

### ランタイムセッション ID

エージェントのランタイムが割り振る会話の ID。Claude Code では UUID、Codex ではスレッド ID。Claude Code では `/clear` と compact のたびに新しい ID になる。SessionChain の末尾が現在の ID。

**型**: `session.RuntimeSessionID`

### SessionChain

1つの Session が経験したランタイムセッション ID の履歴 (古い順)。`/clear` や compact のたびに末尾に新 ID が追加される。

**フィールド**: `Session.SessionChain []RuntimeSessionID`

### ForkedFrom

フォークで作った Session の分岐元のランタイムセッション ID。別の Session の SessionChain の要素を指す。フォークでなければ空。claude-deck の `f` か `claude-deck fork`（どちらも `ForkSession`）で作ったフォークにだけ付き、Claude Code の中で分岐させた会話は記録されない。`claude-deck tree` は、SessionChain と ForkedFrom からランタイムセッション ID の木を組み立てる。

**フィールド**: `Session.ForkedFrom RuntimeSessionID`

## 状態モデル

### Origin

セッションのプロセスを誰が起動したか。

| 値 | 意味 |
|----|------|
| Deck | claude-deck が起動した。store に行がある（deck セッション） |
| External | JSONL から発見した、claude-deck の外で起動したセッション（外部セッション）。メモリだけに持ち、再開すると Deck として取り込む |

**型**: `session.Origin`

### Status

deck セッションの実行状態。動いている間の活動（Activity）か、終わり方。外部セッションかどうかは Status でなく Origin で表す。

| 値 | 意味 |
|----|------|
| Idle | プロセス起動済みだが Claude が処理中でない (Hook turn.complete) |
| Running | Claude が思考/実行中 (Hook turn.start / tool.call) |
| SubagentRunning | メインのターンは終わったが、バックグラウンドのサブエージェントが動いている (Hook turn.complete) |
| WaitingApproval | ツール承認待ち (Hook PermissionRequest) |
| WaitingAnswer | ユーザー質問待ち (Hook PermissionRequest / AskUserQuestion の tool.call) |
| Completed | プロセス終了 (ウィンドウのコマンドの `hook exited` / close / ウィンドウ消失の検知) |
| Error | 再開でワークスペースを作り直せなかった、または終了済みセッションの作業ディレクトリが消えている |
| Unknown | store にこの版の知らない値がある（新しい版の claude-deck が書いた）。動いているとも終わったとも扱わず、hook も再開も prune も行に触れない。外部セッションの Status もこれ |

Idle から WaitingAnswer までの 5 つが活動（**型**: `session.Activity`）で、hook と Codex の JSONL が報告できるのはこれだけ。

遷移の規則は次の 3 つ。

- hook が報告するステータス（Idle、Running、SubagentRunning、WaitingApproval、WaitingAnswer）は、動いているセッションにだけ適用する。動いている間は、この 5 つのどれからどれへも移る。終了済み（Completed / Error）と Unknown の行には適用しない
- 終了は、ウィンドウのコマンドの `hook exited`、`x` と `claude-deck close`、ウィンドウ消失の検知の 3 経路で記録し、Completed にする。すでに Error の行は Error のまま
- Completed と Error から Idle に戻るのは、再開（`r`）と、TUI の起動時にそのセッションの tmux ウィンドウが生きていた場合だけ。外部セッションを再開すると、deck のセッションとして取り込んでから再開する

**型**: `session.Status`

## インフラ

### トランスクリプト（JSONL）

Claude Code が `~/.claude/projects/<project>/<uuid>.jsonl` に書き出すセッションログ。対話のプロンプト、レスポンス、ツール実行を含む。claude-deck の一次データソース。

**関連**: `internal/usage/`

### Store

JSONL が「Claude Code の記録」なのに対し、Store（`{DataDir}/deck.db`）は「deck の記録」で、deck セッションの信頼できる唯一の情報源。外部セッション（Origin が External）は持たない。

### Workspace

jj (Jujutsu) のワークスペース機能で作成される隔離作業ディレクトリ。各セッションが独立したファイルシステム状態を持てる。名前はセッション名と同じで、`{DataDir}/workspace/` の下に作る。

**関連**: `internal/jj/`

### Isolation

deck セッションがどこでプロセスを動かすか。直接起動（リポジトリかサブプロジェクトのディレクトリで、ワークスペースなし）か、そのセッションの Workspace の中か。作った後は変わらない。ワークスペース付きのセッションは、close でワークスペースを消した後もワークスペース付きのままで、再開すると作り直す。外部セッションを取り込むと、cwd が jj ワークスペースでも直接起動になる。いつ作り、いつ消すかは [data-lifecycle.md](data-lifecycle.md#隔離isolation)。

**型**: `session.Isolation`
