# claude-deck

Claude Code セッションを一括管理する TUI ダッシュボード。

## セットアップ

### 必須環境

- Go 1.26+
- `encoding/json/v2` を使用 → **`GOEXPERIMENT=jsonv2` が必須**

### ビルド・テスト・実行

```bash
cd claude-deck
GOEXPERIMENT=jsonv2 go build -o claude-deck ./cmd/claude-deck
GOEXPERIMENT=jsonv2 go test ./...
./claude-deck
```

claude-deck を直したら、`go build` の後に `claude-deck reload` を打ち、動いている TUI を新しいバイナリで起動し直す。起動し直すのは、TUI を起動したパス（リンクならその先）のバイナリで、打った側のビルドではない。出力の `tui_revision` と `cli_revision` が違えば、TUI は今のビルドを動かしていない。`deckmod/` を変えたなら、続けて `claude-deck broadcast -p /reload-plugins` も打ち、動いているセッションにプラグインを読み込み直させる。broadcast は自分を飛ばすので、自分には `claude-deck send --self -p /reload-plugins` を打つ（ターンの後に流れる）。reload は deckmod を書き出し直すだけで、セッションには送らない。

## アーキテクチャ概要

パッケージの一覧と依存の方向は [docs/01-overview.md](docs/01-overview.md) のパッケージマップにある。

## 設計原則

機能追加・設計変更・リファクタリングを頼まれたら、実装の案より先に次の4つをする。

1. この変更が扱うドメインの概念を挙げ、利用者と認識を合わせる
2. 新しい振る舞いをどの型・モジュールに持たせるかを、既存の責務の配分と照らして話し合う
3. 既存の境界（パッケージ、プロセス）を越えるかを確かめる。越えるなら、どう接続するかを決める
4. 実装の詳細でなく、ドメインモデルの水準で設計の案を出し、合意してから実装に入る

ドメインモデルが変わらないバグ修正、1つの関数に閉じた変更、利用者が実装の方法まで指定したときは省いてよい。

`/review-domain` コマンドでドメイン分析・アーキテクチャレビューを実行可能。

## ドキュメント

| ファイル | 書いてあること |
|---|---|
| [docs/00-glossary.md](docs/00-glossary.md) | 用語 |
| [docs/01-overview.md](docs/01-overview.md) | 全体の構成、プロセス、パッケージの一覧 |
| [docs/architecture.md](docs/architecture.md) | プロセスと store の分担、セッションの作成・再開・終了 |
| [docs/concurrency.md](docs/concurrency.md) | ロックの順序とプロセス間の排他 |
| [docs/data-lifecycle.md](docs/data-lifecycle.md) | 外部データ（ワークスペース、store、JSONL、`~/.claude.json`）をいつ作り、いつ消すか |
| [docs/hooks.md](docs/hooks.md) | deck-status プラグインと、Claude Code のイベントとステータスの対応 |
| [docs/session-id.md](docs/session-id.md) | deck ID とランタイムセッション ID の関係、重複を防ぐ規則 |
| [README.md](README.md) | 利用者向けの紹介、始め方、キー（日本語の短い版は README.ja.md） |
| [docs/configuration.md](docs/configuration.md) | `config.toml` の全項目とデータディレクトリ（利用者向け、英語） |

初めて読むときは上から順に読む。

### どこに書くか

| 書くこと | 置き場所 |
|---|---|
| 何を表し、何をするか | 型、関数名、コード |
| 防ぎたい不具合と守る振る舞い | テスト |
| 1 つの実装を変えるときに要る理由、外部の制約（確かめた Claude Code の版を添える）、意図的な非対称 | その実装のコードコメント（`WHY:` / `WHY NOT:`） |
| 複数のパッケージやプロセスにまたがる概念、不変条件、利用者への約束 | docs/ の上の表のファイル |
| 検討して却下した案、変更の経緯、検証の結果 | PR 本文とコミットメッセージ |

- 判断ごとに文書を作らない。1 つの実装に閉じる判断はコードコメントに書く
- 同じ規則は 1 か所にだけ書き、ほかからはリンクする
- 関数、ファイル、列の一覧や、コードを文章に書き写したものは書かない
- 以前の実装や経緯は書かない。文書はいま有効な内容に書き換える
- コードを変えたら、影響する docs/ の記述を同じ PR で直す。1 つの実装に閉じる変更なら文書は直さなくてよい
- 実装前の Design Doc（docs/design/）は、実装が終わったら残す情報を上の置き場所に移して消す

## 開発時の重要事項

### ロック順序（デッドロック防止）

Manager.mu → Session.mu の順で取得すること。逆順は ABBA デッドロックを起こす。詳細は [docs/concurrency.md](docs/concurrency.md)。

### セッション ID

`/clear` でランタイムセッション ID（Claude Code の UUID、Codex のスレッド ID）は変わるが、deck ID（`Session.ID`）は変わらない。hook は環境変数の deck ID で store の行を特定するので、ランタイムセッション ID との突き合わせは要らない。用語は [docs/00-glossary.md](docs/00-glossary.md)、重複を防ぐ規則は [docs/session-id.md](docs/session-id.md)。

起動するセッションには次の環境変数を渡す。

| 環境変数 | 内容 |
|----------|------|
| `CLAUDE_DECK_SESSION_ID` | deck ID |
| `CLAUDE_DECK_DATA_DIR` | データディレクトリ（`config.Load` が `data_dir` の上書きとして読む） |
| `CLAUDE_DECK_BIN` | claude-deck バイナリの絶対パス |

### セッションステータス

`Status` を書くのは `internal/session/transitions.go` の関数だけ。どのプロセスも store のトランザクション内で同じ関数を適用する。値の意味は [docs/00-glossary.md](docs/00-glossary.md)、hook のイベントとの対応は [docs/hooks.md](docs/hooks.md)。

### 項目の書き手

store（SQLite `deck.db`）が信頼できる唯一の情報源で、`Manager.sessions` は store を `Manager.Reload` で読み直したもの。store が書く項目は常に store に従い、JSONL と jj から TUI が読む項目は、セッションが初めてメモリに現れるときだけ store から読む。外部セッションは store に入れない。項目を足すときは、どちらの側かを決めて `internal/session/record.go` の写し方に合わせる。詳細は [docs/architecture.md](docs/architecture.md)。

### キーバインド

キーは [README.md](README.md) の Keybindings にある。キーを変えたら、README と `?` のヘルプ文字列（`internal/tui/keys.go`）を直す。

### CLI サブコマンド

store と tmux を直接操作するので、TUI が起動していなくても実行できる。出力は JSON（`tree` だけテキスト）。書き手と排他は [docs/architecture.md](docs/architecture.md) と [docs/concurrency.md](docs/concurrency.md)。

CLI を変えたら、同梱のスキル `deck-status:claude-deck`（`deckmod/skills/claude-deck/SKILL.md`）も直す。セッション開始時にモデルへ CLI の使い方を伝えるのはこのスキル。

| コマンド | 対応するキー |
|------|------|
| `claude-deck new [--dir DIR] [--no-workspace] [--prompt TEXT\|-]` | `n`（`--no-workspace` は C-Enter）。`--prompt` で最初の指示を渡す（TUI には無い） |
| `claude-deck fork [--session ID\|NAME] [--prompt TEXT\|-]` | `f`。`--session` を省くと自分のセッション（`$CLAUDE_DECK_SESSION_ID`）。`--prompt` で最初の指示を渡す（TUI には無い） |
| `claude-deck list [--alias TEXT] [--repo TEXT] [--only ID\|NAME,...]` | 一覧表示。`session_chain` と `forked_from` で `/clear` とフォークの系譜も返す。`--alias` はエイリアスの部分一致、`--repo` はリポジトリの名前（`repo_path` の末尾）の部分一致、`--only` は ID か名前の完全一致で絞る。付けた条件のどれにも当たるもの（`broadcast` と同じ関数） |
| `claude-deck tree` | なし。Claude Code のセッションを `/clear` とフォークの親子関係でたどった木を、テキストで出す（[用語集](docs/00-glossary.md) の ForkedFrom） |
| `claude-deck close <ID\|NAME>` | `x` |
| `claude-deck reload [--dry-run]` | なし。動いている TUI に、自分のバイナリ（起動したパスのリンクをたどった先）で起動し直すよう頼む。TUI は新しいバイナリの `version` が動くのを確かめてから exec し、カーソルの位置と Ghostty の右ペインを引き継ぐ。動かないとき、操作やダイアログの途中のとき、TUI が動いていないときは、起動し直さずに 0 以外で終わる。取り決めは [docs/architecture.md](docs/architecture.md#tui-の起動し直しreload) |
| `claude-deck version` | なし。ビルドの revision を JSON で出す。reload がバイナリの確かめに使う |
| `claude-deck send --id <ID\|NAME> -p <TEXT> [--dry-run]` | なし。そのセッションの tmux のウィンドウの入力欄に 1 行の文と Enter を打つ。`idle` か `waiting_action` で入力欄が空のときだけ送り、自分のセッションには送らない（`session.CheckSend`） |
| `claude-deck send --self -p <TEXT> [--dry-run]` | なし。自分のセッションの入力欄に打つ。ターンの途中でも送り（文は列に入り、ターンの後に流れる）、承認や回答のダイアログの間と、打ちかけがあるときは断る（`session.CheckSendSelf`）。Mod を直した後の `/reload-plugins` に使う |
| `claude-deck broadcast -p <TEXT> [--alias TEXT] [--repo TEXT] [--only ID\|NAME,...] [--dry-run]` | なし。生きているセッションのうち `list` と同じ絞り込みに合うものへ、`send` と同じ判定で送る。合うものが無ければ 0 以外で止まる |
| `claude-deck clear --id <ID\|NAME> [--dry-run]` | なし。`send -p /clear` の短い形。`list` の `hints` の `context_full` が打つ時機を勧める |
| `claude-deck alias [--session ID\|NAME] <ALIAS>` | なし。セッションに表示用の名前を付ける（英数字と `-` `_` `.`、40 文字まで）。`--session` を省くと自分のセッション（`$CLAUDE_DECK_SESSION_ID`）。空文字で消す |
| `claude-deck gc [--dry-run]` | なし。どのセッションのものでもないワークスペースと、消えたワークスペースについての `~/.claude.json` の登録を消す |
| `claude-deck project list` | なし。`config.toml` の `[projects]` にあるプロジェクトごとの設定を返す |
| `claude-deck project args [--clear] <DIR> [-- <ARG>...]` | なし。そのリポジトリで Claude Code を起動するときに足す引数（`config.toml` の `claude_args`）を読み書きする。store と tmux には触れない |

内部用に `claude-deck hook status|session-start|exited|rate-limits|context --session <ID>` がある。deck-status プラグインとウィンドウのコマンドが呼ぶもので、手で実行するものではない。

`x` と `close` を複数のプロセスが同時に実行したときの排他は [docs/architecture.md](docs/architecture.md) にある。

### deck-status プラグイン

`deckmod/` はバイナリに埋め込まれ、claude-deck の起動時に `{DataDir}/plugin/` へ書き出される。`deckmod/` を変えたらバイナリを作り直す。仕組みとイベントの対応は [docs/hooks.md](docs/hooks.md)。

### データと設定の置き場所

データディレクトリの中身と、それぞれをいつ作り、いつ消すかは [docs/data-lifecycle.md](docs/data-lifecycle.md)。`config.toml` の項目は [docs/configuration.md](docs/configuration.md) にある。項目を足したら、README の Configuration の例に載せるほどよく使うものかも考える。
