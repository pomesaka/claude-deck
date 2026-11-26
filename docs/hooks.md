# deck-status プラグイン

## 仕組み

`deckmod/` の Claude Code プラグイン `deck-status` が、Claude Code のイベントを受けて `claude-deck hook ...` を実行し、store（`deck.db`）を直接書く（レート制限だけは別のファイルに書く）。TUI は store の変更を `PRAGMA data_version` で検知して表示を更新する。TUI が起動していなくても store には書かれる。

- プラグインは Mods の function hook モジュール（`deckmod/hooks/register.ts`）
- バイナリに埋め込まれ、起動時に `{DataDir}/plugin/` へ書き出される
- claude-deck が起動する全セッションに `--plugin-dir {DataDir}/plugin` を渡す
- プラグインは環境変数 `CLAUDE_DECK_BIN` と `CLAUDE_DECK_SESSION_ID` が両方あるセッションでだけ動く。claude-deck 以外で起動した Claude Code では何もしない
- `claude-deck hook` の失敗は握りつぶす。ステータスの報告に失敗しても、ユーザーのターンや承認の流れは止めない
- 呼び出しは直列化する。ほぼ同時に続けて発火するイベントが、古い状態で新しい状態を上書きしないようにするため
- 直前に書けたステータスと同じなら書かない。`tool.call` はツールごとに発火し、書き込みのたびにプロセスが起動して TUI が再描画されるため。書き込みに失敗したときは記録しないので、次の同じステータスで書き直す

deck ID は環境変数から取るので、ランタイムセッション ID との突き合わせは要らない。

## セッションへの案内とスキル

プラグインは、セッションが claude-deck の CLI を使えるようにする役目も持つ。

- `classic.SessionStart`（`agent_id` なし）の結果の `additionalContext` に、claude-deck が起動したセッションであること、deck ID、CLI の絶対パスを足す。`/clear` と compact でも発火するので、文脈が消えた後にも届く
- CLI の使い方は同梱のスキル `deck-status:claude-deck`（`deckmod/skills/claude-deck/SKILL.md`）に書く。案内には、使う前にこのスキルを読むことだけを書く

CLI のサブコマンドやフラグを変えたら、スキルも同時に直す。

## イベントとステータスの対応

| Claude Code のイベント | store への書き込み | 備考 |
|---|---|---|
| `classic.SessionStart`（`agent_id` なし） | `hook session-start --claude-session-id <id> --source <source>` | SessionChain を更新する。モデルに claude-deck の案内を渡す |
| `turn.start`（メイン） | `hook status running`。待ちの理由を消す | ほかの Mod が `prompt.submit` で止めた入力では発火しない（Claude Code 2.1.327 で確認） |
| `turn.start`（サブエージェント） | メインのターンが終わった後で、待ち状態でも `waiting_action` でもなければ `subagent_running` | サブエージェントの実行でも発火する（同じ版で確認）。Mod がメインの停止中に立てたエージェントは、メインの `turn.complete` を通らない |
| `tool.call`（メイン） | `running`（待ち状態のときは変えない）。`AskUserQuestion` のときは `waiting_answer`。実行中のツール呼び出しがすべて返ったら `running` | `next(e)` は承認ダイアログと質問への回答を待つので、返った時点でユーザーが答えている。並行して走る別の呼び出しの承認ダイアログが開いている間は、待ち状態を消さない |
| `tool.call`（サブエージェントと、Mod が起こした呼び出し） | 実行中のツール呼び出しがすべて返り、直前が `waiting_approval` / `waiting_answer` なら `running`。メインのターンが終わった後なら、動いているサブエージェントの有無で `subagent_running` か `idle` | サブエージェントの承認ダイアログもユーザーを待たせる。Mod の `$.agent.spawn` は `agentId` の無い `Agent` の呼び出しとして、メインのターンの外でも届く。`tool_use_id` が `toolu_plugin_` で始まることで見分ける（Claude Code 2.1.327 で確認） |
| `classic.PermissionRequest` | `waiting_approval`。`AskUserQuestion` のときは `waiting_answer` | サブエージェントでも書く |
| `turn.complete`（メイン） | 待ちの理由があれば `waiting_action --reason <理由>`。無ければ `idle`、動いているサブエージェントがあれば `subagent_running` | 拒否・中断・API エラーでも発火する。サブエージェントは `$.agent.list()` の `pending` / `running` / `waiting` を数える |
| `turn.complete`（サブエージェント） | 直前が `subagent_running` で、動いているサブエージェントが無くなったら `idle` | メインのターンが終わった後も、バックグラウンドのサブエージェントは動き続ける |
| `classic.Stop`（メイン） | 書かない。最後の返答から待ちの理由を作り、プラグインのメモリに置く。メインのターンが既に終わっていたら、`turn.complete` と同じ規則で書く | 下の「利用者の操作待ち」 |
| `classic.SessionStart`（source が `clear`） | 待ちの理由があれば消して、`idle` か `subagent_running` | 頼んだ会話が消えるので、待ちも消す |
| `session.measure`（`changed` に `rateLimits` があるとき） | `hook rate-limits <JSON>` | store ではなく `{DataDir}/rate-limits.json` に書く（下の節） |

サブエージェントとして数えるのは `$.agent.list()` に出るエージェントだけ。バックグラウンドのシェルとモニターは数えないので、それだけが動いている間は `idle` になる。ワークフローのエージェントも一覧に出ない（Mods の型定義 `AgentLoop.agentId` の説明。動作は未確認）。

終了は別経路で、プラグインは関与しない（[ウィンドウの終了検知](architecture.md#プロセス終了の検知)）。

`session-start` の source ごとの SessionChain の更新は [session-id.md](session-id.md#sessionstart-の-source) にある。

## 利用者の操作待ち

モデルが `! コマンド` の実行を頼んでターンを終えると、`idle` では仕事が終わったのか待っているのかが分からない。そこでプラグインが、メインの `classic.Stop` の `last_assistant_message` から頼みを見つけ、ターンの後の状態を `waiting_action` にする。

- 頼みと見なすのは、行頭の `! ` に ASCII のコマンド名が続く行だけ。コードブロックの中の行も見る。`! コマンド` はコードブロックで示すのがふつうの頼み方なので
- バッククォートの中の `! `、`! <コマンド>` のような書き方の説明、「ログインしてください」のような文は見ない。説明や引用を頼みと取り違えるため。モデルにツールで申告させる作りは、待つだけのターンでも呼ばれて利用者を誤解させたのでやめた
- 理由の文は、見つけた最初のコマンドから「`` `! コマンド` ``の実行を待っています」の形で作る。一覧の 2 行目の末尾に出すので 80 文字までにし、超えたらコマンドを切って `…` を付ける
- `classic.Stop` の時点では store に書かない。メインの `turn.complete` で `idle` の代わりに `waiting_action` を書く
- バックグラウンドのサブエージェントが動いていても `waiting_action` を出す。利用者にすることがあるほうを優先する
- 次のメインの `turn.start`（利用者の次の発言）と `/clear` で外す。`! コマンド` の実行で `turn.start` が発火するかは未確認

## レート制限

TUI のヘッダーに出す 5 時間枠と 7 日枠の使用率は、プラグインが `session.measure` で受け取って報告する。

- `session.measure` は、メインスレッドのターンが終わるたびと、枠が 1 ポイント動いたときに発火する（Claude Code 2.1.295 の型定義と実機で確認）。`changed` に `rateLimits` が無いときと、枠が 1 つも無いとき（サブスクリプションでないとき）は報告しない
- 引数の JSON は `rateLimits` の配列（`kind`・`percentUsed`・`resetsAt`）をそのまま文字列にしたもの。`ratelimits.ParseMeasured` が読む
- アカウント単位の値なので store の行には入れない。どのセッションも `{DataDir}/rate-limits.json` に書き、最後の報告が残る。TUI は `ratelimits.Watch` でこのファイルを監視する。監視を始めた時点の中身も読むので、起動してすぐに前回の値が出る
- claude-deck の外で起動したセッションは報告しない
- status line の入力（`rate_limits`）からは取らない。取るには利用者の `statusLine` を claude-deck のコマンドで置き換えることになり、`--settings` でセッションごとに渡しても置き換えは変わらない

## なぜ Stop でなく turn.complete か

承認ダイアログで拒否したターンは、`Stop`・`PermissionDenied`・`PostToolUseFailure` のどれも発火しない。`Stop` で Idle に戻すと、拒否後のセッションが Waiting のまま残る。`turn.complete` は拒否・中断・API エラーのどれでも発火する（Claude Code 2.1.287 で確認）。

## なぜ tool.check の ask でなく PermissionRequest か

auto モードでは `tool.check` が ask を返しても承認ダイアログを出さずに実行される。ダイアログが出たときだけ `PermissionRequest` が発火する（Claude Code 2.1.287 で確認）。

## ステータス更新のルール

`applyHookStatus` が store の行に適用する。

- 終了済み（Completed / Error）の行には適用しない。終了の記録より後に届いた hook が、セッションを生き返らせないようにするため
- この版の知らない status（Unknown）の行にも適用しない。新しい版の claude-deck が書いた行を上書きしないため。外部セッションは store に行が無いので、hook は届かない
- 現在のステータスと同じなら何もしない
