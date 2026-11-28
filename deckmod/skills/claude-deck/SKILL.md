---
name: claude-deck
description: claude-deck の CLI で、別の Claude Code セッションを作る・フォークする・一覧する・閉じる、セッションに表示用の名前を付ける。 - 「別セッションを立てて」「この会話を引き継いだ分身を立てて」「新しいワークスペースで始めたい」「いま動いているセッションを見せて」「あのセッションを閉じて」「このセッションに名前を付けて」と言われたとき。
---

# claude-deck

claude-deck は、Claude Code のセッションを tmux のウィンドウと jj ワークスペースで管理するダッシュボード。このセッションも claude-deck が起動している。

CLI は store と tmux を直接操作するので、TUI が起動していなくても動く。バイナリは PATH に無いことがあるので、環境変数 `CLAUDE_DECK_BIN` の絶対パスで呼ぶ（セッション開始時の案内に実際のパスが書かれている）。

## コマンド

| コマンド | 動作 |
|---|---|
| `new [--dir DIR] [--no-workspace] [--prompt TEXT\|-]` | 新しいセッションを作り、tmux のウィンドウで Claude Code を起動する。`--prompt` の文を最初の指示として渡す |
| `fork [--session ID\|NAME] [--prompt TEXT\|-]` | セッションの会話を引き継いだ新しいセッションを、新しいワークスペースで起動する。`--session` を省くと自分をフォークする。`--prompt` の文を最初の指示として渡す |
| `list [--alias TEXT]` | claude-deck のセッションを TUI の一覧と同じ順で返す。claude-deck の外で起動したセッションは含まない。`--alias` を付けると、エイリアスにその文字列を含むものだけを返す |
| `list --only ID\|NAME,...` | 挙げたセッションだけを返す。ID か名前に、そのまま一致するもの（`send --id` と同じ指し方）。`broadcast` では、当たらない名前と、2 つ以上に当たる名前があると何も送らずに止まる |
| `list --repo TEXT` | リポジトリの名前（`repo_path` の末尾）にその文字列を含むセッションだけを返す。大文字と小文字を区別しない。`--alias` と組み合わせると両方に当たるものだけを返す |
| `tree` | Claude Code のセッションを、`/clear` とフォークの親子関係でたどった木としてテキストで出す |
| `close <ID\|NAME>` | プロセスを止め、ワークスペースを消す。JSONL とメタデータは残り、TUI から再開できる。jj が作業コピーを取り込めなかったときはワークスペースを残し、stderr に警告を出す |
| `send --id ID\|NAME -p TEXT [--dry-run]` | そのセッションの入力欄に 1 行の文を打って Enter を送る。利用者が打ったのと同じ形で届く。送るのは `idle` か `waiting_action` で、入力欄に打ちかけの文が無いときだけ。それ以外と自分のセッションには送らず、理由を出して 0 以外で終わる |
| `send --self -p TEXT [--dry-run]` | 自分のセッションの入力欄に打つ。ターンの途中でも打て、Claude Code が列に入れてターンの後に流す。承認や回答のダイアログの間と、打ちかけの文があるときは断る。Mod を直した後に `send --self -p /reload-plugins` で自分も読み込み直す |
| `broadcast -p TEXT [--alias TEXT] [--repo TEXT] [--only ID\|NAME,...] [--dry-run]` | 生きているセッションのうち絞り込みに合うものに、`send` と同じ判定で同じ文を送る。絞り込みは `list` と同じで、条件はどれも満たすものだけ。セッションごとの `{id, name, sent, skipped_reason, dry_run}` の配列を返す。合うものが無ければ何も送らずに 0 以外で終わる |
| `clear --id ID\|NAME [--dry-run]` | `send -p /clear` の短い形。文脈を空にする |
| `alias [--session ID\|NAME] <ALIAS>` | セッションに表示用の名前を付ける。TUI の一覧で、セッション名と並べて出る |
| `gc [--dry-run]` | どのセッションのものでもないワークスペースと、消えたワークスペースについての Claude Code の記録を消す |
| `reload [--dry-run]` | 動いている TUI を、そのバイナリ（TUI を起動したパス）で起動し直す。claude-deck をビルドし直した後に打つ。新しいバイナリが動かないときと、TUI が操作の途中のときは起動し直さず、0 以外で終わる。セッションは止まらない。出力の `tui_revision` と `cli_revision` が違えば、TUI は打った側のビルドを動かしていない |
| `sent [--since TIME]` | claude-deck が利用者の代わりに打った文（`new` と `fork` の `--prompt`、`send`、`broadcast`）の記録を、古い順に返す。スラッシュコマンドは記録しない。`{session_id, claude_session_id, sent_at, source, text_sha256}`。`--since` は RFC 3339 か、今からさかのぼる長さ（`24h`） |
| `project list` | 設定ファイルにある、プロジェクトごとの設定を返す |
| `project args [--clear] <DIR> [-- <ARG>...]` | そのリポジトリで Claude Code を起動するときに足す引数を読み書きする |

出力は JSON。`new`・`fork`・`close`・`alias` は次の形で 1 件、`list` はその配列を返す。

```json
{
  "id": "deck のセッション ID",
  "name": "セッション名",
  "alias": "alias で付けた表示用の名前（付けていなければ無い）",
  "repo_path": "リポジトリのルート",
  "work_dir": "Claude Code が動いているディレクトリ",
  "status": "idle | running | subagent_running | waiting_approval | waiting_answer | waiting_action | completed | error",
  "status_reason": "waiting_action のとき、利用者がすること。実行する `!` のコマンドか、API の誤りで止まったこと（ほかの状態では無い）",
  "claude_session_id": "Claude Code のセッション ID（まだ届いていなければ無い）",
  "session_chain": ["/clear をまたいだ Claude Code のセッション ID。古い順で、末尾が claude_session_id"],
  "forked_from": "フォークで作ったセッションの、分岐元の Claude Code のセッション ID（別のセッションの session_chain の要素。フォークでなければ無い）",
  "context": { "used_tokens": "Claude Code が測った、最後の応答の入力のトークン数", "window_tokens": "モデルの文脈の窓の大きさ" },
  "compactions": "いまの会話で compaction が走った回数（/clear で 0 に戻る）",
  "hints": [{ "kind": "context_full | api_error | stale | unsaved", "message": "次の一手の案" }]
}
```

- `context` と `compactions` は、測っていなければ `null`（0 とは別）。`hints` は無ければ `[]`
- `hints` は `list` のたびに状態と測った値から出す。`context_full` は文脈が半分以上詰まったか compaction が走ったセッションで、きりのいい所で `clear` を打つ。`api_error` は API の誤りで止まったセッションで、SendMessage で何か送ると再開する。`stale` は 2 日動いていないセッションで、要らなければ `close` する。`unsaved` は `stale` のセッションのワークスペースに、未コミットの変更か push していないコミットがあるときに続けて出る。閉じる前に、入れるか要らないかを利用者に確かめる

`tree` だけはテキストを返す。系譜をプログラムから読むときは、`list` の `session_chain` と `forked_from` を使う。

## new

- `--dir` を省くとカレントディレクトリで起動する。jj ワークスペースの中を指定すると、本体リポジトリの同じ位置に読み替える
- 既定では jj ワークスペースを新しく作り、その中で起動する。jj リポジトリでないディレクトリはエラーになるので、`--no-workspace` を付けて直接起動する
- `--prompt TEXT` で最初の指示を渡せる。長い指示や改行を含む指示は `--prompt -` にして標準入力から渡す。相手はこちらの会話を知らないので、目的、対象のファイル、終わったら何を返してほしいかを書く。付けなければ入力待ちで始まる
- 作った後は `name` と `work_dir` を利用者に伝える
- `new` は相手が指示に取りかかる前に戻り、そのときの `status` はもう `idle` になっている。指示の完了を待つときは、`running` になったのを見てから `idle` を待つ
- 新しいワークスペースは、呼び出し元の作業コピーではなく `trunk()` の上に作られる（`jj git fetch` の後に `jj new trunk()`）。呼び出し元の未コミットの変更と、`trunk()` に入っていないコミットは相手に見えない。相手に読ませたいファイルがそこにあるなら、先に `trunk()` へ入れるか、依頼文に中身を書く

## fork

- 会話の文脈を引き継いだ分身に、計測や調査を任せるときに使う。相手はこちらの会話を最後まで知っているので、`new` と違い、依頼文に経緯を書き直さなくてよい。何を任せたか、終わったら何を返すか（SendMessage で返す）は `--prompt` に書く。相手は自分がフォークされた側だと知らないので、「あなたはフォークされた分身で、元のセッションは <name>」と書く
- `--session` には ID か `name` を渡す。省くと自分（環境変数 `CLAUDE_DECK_SESSION_ID`）をフォークする
- 元のセッションにまだ会話が無いとエラーになる
- ワークスペースは `new` と同じく `trunk()` の上に新しく作られる。元のセッションの作業コピーにある未コミットの変更は、会話では知っていても相手のファイルには無い
- `--prompt` の規則と、戻ったときの `status` は `new` と同じ。作った後の頼み方は次の節と同じ

## 作ったセッションに作業を頼む

`new` が返す `name` は、Claude Code のセッション名と同じ。SendMessage の `to` にそのまま指定すると、そのセッションにメッセージが届く。既に動いているセッション（`list` の `name`）にも同じように送れる。

- 相手には利用者の入力としてではなく、別セッションからのメッセージとして届く。相手は自分の権限設定の範囲でだけ動くので、こちらで拒否された操作を代わりに頼まない
- 相手はこちらの会話を知らない。目的、対象のファイル、終わったら何を返してほしいかをメッセージに書く
- 起動が済む前に送ると「宛先が見つからない」で失敗する。そのときは ListAgents に `name` が出てから送り直す
- 終わったことを知りたいときは `notify_when_idle` を付ける。ListAgents を繰り返して待たない
- 待機の通知は完了の合図ではない。相手が裏で走らせたコマンドの終了を待っているあいだにも届く。通知が来たのに報告が届いていなければ、`list` の `status` で相手の状態を確かめる
- `status` が `idle` で報告が届いていなければ、相手は止まっている。報告を自分の画面に書いただけで終えていることが多いので、SendMessage で「報告が届いていない」と伝えて送り直させる。`running` なら作業中なので待つ
- 相手が画面に書いた文はこちらに届かない。依頼文に「結果は SendMessage で返す」と書く

## send と broadcast

- **頼まれていないのに、ほかのセッションの入力欄へ打ち込まない**。打った文は、利用者がそのセッションで打った発言と同じ形で届き、相手は利用者の指示として動く
- ふつうの依頼と報告の催促は SendMessage で送る。`send` と `broadcast` は、`/reload-plugins` や `/clear` のような、入力欄でしか効かないコマンドと、利用者に頼まれた文のためにある
- `broadcast` の前に、同じ絞り込みの `list` か `--dry-run` で宛先を確かめる。`--dry-run` の出力で、`skipped_reason` が空のものが送る先、入っているものが飛ばす先
- 文は 1 行だけ。改行を含む文と空の文は断られる
- `broadcast` は自分を飛ばす。自分に打つのは `send --self` を明示したときだけ

## 会話の記録から利用者の発言だけを取る

claude-deck が打った文も、Claude Code の会話の記録では利用者が打った発言（origin が human）になる。記事やまとめに利用者の言葉として引く前に、次のように外す。

1. 記録の user の行の文を、そのまま SHA-256 にする
2. `sent` の行のうち、`claude_session_id` がその記録のセッションで、`text_sha256` が同じものがあれば、claude-deck が打った文として外す
3. フォークした会話の記録は、元の会話の行から始まる。フォークの記録では、`forked_from` のセッションの行とも照らす
4. スラッシュコマンド（`clear`、`-p /reload-plugins`）は記録の上で `<command-name>` の行になり、`sent` には出ない。利用者の発言として引かない

## clear

- 担当のセッションの `hints` に `context_full` が出たら、報告を受け取って次の仕事を頼む前に打つ。仕事の途中で打つと、相手は経緯を失う
- 打った後の相手は何も覚えていない。次の依頼には、目的、対象のファイル、返してほしいものを書き直す
- 断られたら（`running` など）、相手が止まってから打ち直す

## alias

- `--session` を省くと、自分のセッション（環境変数 `CLAUDE_DECK_SESSION_ID`）に付ける
- **自分のセッションのエイリアスは、頼まれなくても付けて、こまめに付け直す**。利用者は一覧のエイリアスを見て、目当てのセッションを探す。古いままだと探せない
  - 最初の依頼で作業の内容が分かった時点で付ける
  - 作業の内容が変わったら付け直す（レビューが終わって実装に入った、別の issue に移った、など）
  - 何をしているセッションかが分かる短い名前にする。対象と作業を入れる（`review-pr-12`、`fix-login-bug`、`deck-alias-feature`）。`work` や `task` のような、どのセッションにも当てはまる名前は付けない
  - 付けたことを利用者に報告しなくてよい
- 使えるのは英数字と `-` `_` `.` だけで、40 文字まで（`review-pr-12`、`fix-login-bug` など）。日本語と空白は使えない
- 付け直すと上書きされ、`""` を渡すと消える
- フラグは名前より前に書く（`alias --session anna-8cc7 review-pr-12`）
- 変わるのは表示だけ。`name` は変わらないので、SendMessage の宛先と `close` の指定には `name` を使う

## エイリアスで指されたセッションに頼む

利用者は、セッションをエイリアスで呼ぶ（「`review-pr-12` に聞いて」「あの `fix-login` のセッションに頼んで」）。SendMessage の宛先はエイリアスではなく `name` なので、先に対応する `name` を調べる。

```bash
"$CLAUDE_DECK_BIN" list --alias review-pr-12 | jq -r '.[] | "\(.name)\t\(.alias)\t\(.status)"'
```

- `--alias` は部分一致で、大文字と小文字を区別しない。利用者がうろ覚えの名前を言ったときは、短い語で探す
- 1 件に決まったら、その `name` を SendMessage の宛先にする
- 複数見つかったら、推測で選ばずに候補（`name`、`alias`、`work_dir`）を利用者に見せて確かめる
- 0 件なら、利用者の言った語が `name`（`shoko-677b` のような形）のことかもしれない。`list` の全件から `name` で探す
- `status` が `completed` か `error` のセッションは動いていないので、送っても届かない。利用者に、TUI で再開するよう伝える

## close

- ワークスペースを消すので、利用者に頼まれたセッションだけを閉じる
- 自分のセッション（環境変数 `CLAUDE_DECK_SESSION_ID`）を閉じると、この Claude Code も止まる
- 名前で指定する前に `list` で対象を確かめる

## gc

- 消したものは戻せないので、利用者に頼まれたときだけ実行する。先に `--dry-run` で対象を見せる
- 対象は、一覧に無いセッションのワークスペース（上限を超えて一覧から消えたものなど）。`list` に名前があるセッションのワークスペースは消さない

## project

設定ファイル（`config.toml`）の `[projects."<リポジトリ>"]` を読み書きする。`project args` は 1 件、`project list` はその配列を返す。

```json
{
  "path": "リポジトリのルート",
  "workspace_symlinks": ["新しいワークスペースへ symlink するファイル（無ければ無い）"],
  "add_dirs": ["--add-dir で渡すディレクトリ（無ければ無い）"],
  "claude_args": ["Claude Code の起動時に足す引数"]
}
```

- `project args <DIR> -- <ARG>...` で引数を置き換え、`project args --clear <DIR>` で消し、`project args <DIR>` で今の設定を返す。Claude Code へ渡す引数は必ず `--` の後ろに書く
- `DIR` は、`new --dir` と同じ規則でリポジトリのルートに読み替える
- 設定は、次に起動・再開・フォークするセッションから効く。動いているセッションの引数は変わらないので、効かせたいセッションは閉じて再開する
- 利用者の設定ファイルを書き換えるので、頼まれたときだけ実行する

## CLI に無い操作

再開とウィンドウへの移動は TUI のキー（`r` / `Enter`）でだけできる。頼まれたら、TUI で操作するよう伝える。

`hook` サブコマンドはプラグインとウィンドウのコマンドが呼ぶ内部用なので、手で実行しない。
