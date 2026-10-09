# ADR 013: レート制限を deck-status プラグインから受け取る

## ステータス

採択済み

## コンテキスト

TUI のヘッダーに出すレート制限（5 時間枠と 7 日枠）は、Claude Code の status line から取っていた。claude-deck は起動時にラッパースクリプト `{DataDir}/statusline.sh` を書き、`~/.claude/settings.json` の `statusLine` をそのスクリプトに書き換えていた。スクリプトは status line の入力から `rate_limits` を `rate-limits.json` に書き出し、利用者が元から設定していたコマンドがあれば続けて呼んでいた。

この方法には次の問題があった。

- 利用者のグローバルな設定を claude-deck が書き換える。`settings.json` を dotfiles のリポジトリで管理していると、claude-deck のデータディレクトリを指す行がそこに入る
- 利用者の status line の表示が、claude-deck のスクリプトを経由するようになる
- スクリプトが `jq` に依存する

Mods には `session.measure` というイベントがあり、status line と同じ数字（`rateLimits`・`context`・`cost`）が届く。メインスレッドのターンが終わるたびと、レート制限の枠が 1 ポイント動いたときに発火する（Claude Code 2.1.295 の型定義と実機で確認）。

## 決定

deck-status プラグインが `session.measure` を受け、`changed` に `rateLimits` が含まれるときに `claude-deck hook rate-limits <JSON>` を実行する。JSON は `rateLimits` の配列をそのまま文字列にしたもので、`ratelimits.ParseMeasured` が読み、`ratelimits.Save` が `rate-limits.json` に書く。ファイルの形式と TUI の監視（`ratelimits.Watch`）は変えない。Codex の経路も同じ `Save` で書いている。

レート制限はアカウント単位の値なので、store の行には持たせない。どのセッションも同じファイルに書き、最後の報告が残る。

`SetupStatuslineHook` は消し、代わりに `RestoreStatusLine` を起動時に呼ぶ。`statusLine` が claude-deck のスクリプトを指していれば、スクリプトが連鎖していた元のコマンドに戻し（無ければ `statusLine` を消し）、スクリプトを消す。スクリプトが無ければ何もしない。

## 結果

### 良い点

- `~/.claude/settings.json` を書き換えなくなる。claude-deck がグローバルな設定に足すのは、`~/.claude.json` の trust の登録だけになる
- `jq` が要らなくなる
- 報告は枠が 1 ポイント動いたときだけなので、書き込みの回数が減る

### 悪い点

- 報告するのは claude-deck が起動したセッションだけになる。claude-deck の外で起動したセッションしか動いていない間は、ゲージが古い値のまま残る
- `RestoreStatusLine` は移行のためのコードで、古い版を使っていた環境が無くなれば要らない

### 却下した代替案

- **status line のまま、設定の書き換えだけをやめる**: `--settings` で status line をセッションごとに渡す方法がある。利用者の status line を置き換えることは変わらず、連鎖の仕組みも残る
- **レート制限を store に持つ**: セッションの行に入れると、アカウント単位の値がセッションの数だけ重複する。専用のテーブルを作るほどの量でもなく、ファイルの監視が既にある
- **`settings.json` を map に読んで書き戻す**: キーの順序と整形が変わる。手で管理されているファイルなので、`statusLine` の部分だけをバイト列で差し替える
