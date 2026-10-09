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

deck ID は環境変数から取るので、Claude Code のセッション ID との突き合わせは要らない。

## セッションへの案内とスキル

プラグインは、セッションが claude-deck の CLI を使えるようにする役目も持つ。

- `classic.SessionStart`（`agent_id` なし）の結果の `additionalContext` に、claude-deck が起動したセッションであること、deck ID、CLI の絶対パスを足す。`/clear` と compact でも発火するので、文脈が消えた後にも届く
- CLI の使い方は同梱のスキル `deck-status:claude-deck`（`deckmod/skills/claude-deck/SKILL.md`）に書く。案内には、使う前にこのスキルを読むことだけを書く

CLI のサブコマンドやフラグを変えたら、スキルも同時に直す。

## イベントとステータスの対応

| Claude Code のイベント | store への書き込み | 備考 |
|---|---|---|
| `classic.SessionStart`（`agent_id` なし） | `hook session-start --claude-session-id <id> --source <source>` | SessionChain を更新する。モデルに claude-deck の案内を渡す |
| `turn.start` | `hook status running` | サブエージェントの実行では発火しないので、メインループの開始を表す |
| `tool.call`（メイン） | `running`（待ち状態のときは変えない）。`AskUserQuestion` のときは `waiting_answer`。実行中のツール呼び出しがすべて返ったら `running` | `next(e)` は承認ダイアログと質問への回答を待つので、返った時点でユーザーが答えている。並行して走る別の呼び出しの承認ダイアログが開いている間は、待ち状態を消さない |
| `tool.call`（サブエージェント） | 実行中のツール呼び出しがすべて返り、直前が `waiting_approval` / `waiting_answer` なら `running` | サブエージェントの承認ダイアログもユーザーを待たせる |
| `classic.PermissionRequest` | `waiting_approval`。`AskUserQuestion` のときは `waiting_answer` | サブエージェントでも書く |
| `turn.complete`（メイン） | `idle` | 拒否・中断・API エラーでも発火する |
| `session.measure`（`changed` に `rateLimits` があるとき） | `hook rate-limits <JSON>` | store ではなく `{DataDir}/rate-limits.json` に書く（下の節） |

終了は別経路で、プラグインは関与しない（[ウィンドウの終了検知](architecture.md#プロセス終了の検知)）。

### SessionStart の source

`applySessionStart`（`internal/session/transitions.go`）が source ごとに SessionChain を更新する。

| source | 動作 |
|---|---|
| `startup` / `resume` / `fork` | SessionChain が空のときだけ ID を追加する。再開では、チェーンの末尾と同じ ID が届くので無視する |
| `clear` / `compact` | 末尾と異なる ID なら追加する |

## レート制限

TUI のヘッダーに出す 5 時間枠と 7 日枠の使用率は、プラグインが `session.measure` で受け取って報告する（[ADR-013](adr/013-rate-limits-from-plugin.md)）。

- `session.measure` は、メインスレッドのターンが終わるたびと、枠が 1 ポイント動いたときに発火する。`changed` に `rateLimits` が無いときと、枠が 1 つも無いとき（サブスクリプションでないとき）は報告しない
- 引数の JSON は `rateLimits` の配列（`kind`・`percentUsed`・`resetsAt`）をそのまま文字列にしたもの。`ratelimits.ParseMeasured` が読む
- アカウント単位の値なので store の行には入れない。どのセッションも `{DataDir}/rate-limits.json` に書き、最後の報告が残る。TUI は `ratelimits.Watch` でこのファイルを監視する
- claude-deck の外で起動したセッションは報告しない

## なぜ Stop でなく turn.complete か

承認ダイアログで拒否したターンは、`Stop`・`PermissionDenied`・`PostToolUseFailure` のどれも発火しない。`Stop` で Idle に戻すと、拒否後のセッションが Waiting のまま残る。`turn.complete` は拒否・中断・API エラーのどれでも発火する（Claude Code 2.1.287 で確認、ADR-011）。

## なぜ tool.check の ask でなく PermissionRequest か

auto モードでは `tool.check` が ask を返しても承認ダイアログを出さずに実行される。ダイアログが出たときだけ `PermissionRequest` が発火する（Claude Code 2.1.287 で確認、ADR-011）。

## ステータス更新のルール

`applyHookStatus` が store の行に適用する。

- 終了済み（Completed / Error）と外部セッション（Unmanaged）には適用しない。終了の記録より後に届いた hook が、セッションを生き返らせないようにするため
- 現在のステータスと同じなら何もしない
