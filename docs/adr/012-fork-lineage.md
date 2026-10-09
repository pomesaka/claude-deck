# ADR 012: フォークの分岐元を記録し、セッションの木を出す

## ステータス

採択済み

## コンテキスト

claude-deck は、`/clear` をまたいだ ClaudeSessionID の並びを `SessionChain` として持っている。一方、フォーク（`f`）は元の ClaudeSessionID を `--resume <id> --fork-session` に渡すだけで、どのセッションから分かれたかをどこにも残していなかった。そのため、Claude Code のセッションを親子関係でたどった木を出せなかった。

分岐元を後から復元する手段も調べた（2026-10-09、Claude Code 2.1.295）。

- JSONL: `~/.claude/projects` の 48 ファイルに、ファイル名と違う `sessionId` を持つ行は無かった。フォーク先のファイルから元の ID を読み取る方法は見つかっていない
- hook: Mods の型定義を `fork` と `parent` で探した範囲では、親セッションを伝える項目は無かった。実機では未検証

## 決定

フォークで作るセッションの行に、分岐元の ClaudeSessionID を `forked_from` として書く。値は `ForkSession` が起動の引数に使うのと同じ ID で、分岐元セッションの `SessionChain` の要素を指す。

木の節は ClaudeSessionID（1 つの会話の文脈）にする。節の親は、同じ deck セッションの 1 つ前の ID か、フォークの最初の ID なら `forked_from` の ID になる（`session.BuildTree`）。

- `claude-deck tree`: 木を人向けのテキストで出す
- `claude-deck list`: JSON に `session_chain` と `forked_from` を足す。Claude やスクリプトはこちらを読む

store の列の追加は、`addedColumns` に並べた列を `migrate` が `ALTER TABLE` で足す形にした。これまでは `CREATE TABLE IF NOT EXISTS` だけで、既存の `deck.db` に列を足す手段が無かった。

## 結果

### 良い点

- 分岐元のセッションがその後 `/clear` で先へ進んでも、分かれた位置が変わらない
- 木の組み立ては store の行だけで済み、JSONL を読まない

### 悪い点

- この変更より前に作ったフォークは、分岐元の記録が無いので根として出る
- 分岐元の行が prune されると、`forked_from` は指す先が無くなる。そのセッションは根として出し、分岐元が一覧に無いことを表示する
- `SessionChain` は `/clear` と、ID が変わった compact を区別して持たない。木ではどちらも `/clear` と表示する
- deck を通さない分岐（Claude Code の中で利用者が分岐させたもの）は拾わない
- 列を足す前のバイナリは `INSERT OR REPLACE` で自分の知る列だけを書く。古い TUI が動いている間は、その TUI が書いた行の `forked_from` が空に戻る。バイナリを更新したら TUI を起動し直す

### 却下した代替案

- **分岐元を deck の ID で持つ**: 分岐元のセッションは `/clear` で `SessionChain` が伸びるので、deck の ID だけではどの文脈から分かれたかが分からない。分岐時点の ClaudeSessionID を別に持つ必要があり、それなら ClaudeSessionID だけで足りる（deck の ID は `SessionChain` から引ける）
- **JSONL から分岐元を復元する**: 上のとおり、読み取る方法が見つかっていない
- **`claude-deck session tree` にする**: 今のサブコマンドは `new`・`list`・`close`・`gc` と平らに並んでいて、`session` という階層がこのコマンドだけに付く
- **`claude-deck list --tree` にする**: フラグの有無で出力が JSON とテキストに分かれる
