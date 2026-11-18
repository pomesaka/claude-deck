---
name: verify
description: claude-deck の TUI と CLI を、本物のデータと tmux から隔てた環境で起動し、別の tmux サーバーの中でキーを送って画面を取り、偽の Claude Code でセッションを作って閉じ、変更が動くかを確かめる手順。
when_to_use: TUI（internal/tui）、リポジトリの探索、起動の流れ、セッションの作成と終了（close、x、ワークスペース）、右ペインの切り替えを変えた後に、テストでなく実際に動かして確かめるとき。
---

# claude-deck を動かして確かめる

テストと型検査は、ここでは走らせない。証拠は `tmux capture-pane` で取った画面と、`fd.log` などの外から見える記録。

## 起動する

コマンドは 1 つずつ別の呼び出しで打てるよう、パスを書き切ってある。作業フォルダは `/tmp/cdv` にしている。並べて確かめるなら、フォルダとソケットの名前（`-L cdverify`）を担当ごとに変える。

```bash
sh .claude/skills/verify/setup.sh /tmp/cdv
/tmp/cdv/tmux.sh -L cdverify new-session -d -s run -x 160 -y 40 "/tmp/cdv/run.sh; sleep 600"
```

- tmux サーバーを起動する最初の `new-session` は、`tmux` でなく `/tmp/cdv/tmux.sh` で打つ。サーバーは起動したクライアントの環境を、アプリが作るウィンドウに引き継ぐ。claude-deck の中のセッションから `tmux` で起動すると、`__preview__` が本物の `CLAUDE_DECK_DATA_DIR` と HOME で動き、本物のセッションを表示する。2 つめ以降の操作は `tmux -L cdverify` でよい。

- `setup.sh` は、偽の HOME、遅くて出力が逆順の `fd`、隔てた `config.toml`、`run.sh` を作り、作業コピーを `/tmp/cdv/claude-deck` にビルドする。リポジトリの `./claude-deck` は利用者が使っているので、上書きしない。
- 隔てる理由: 本物の HOME で起動すると、`~/.claude.json` にデータディレクトリの trust を書き、`~/.claude/settings.json` を書き換えることがある。`CLAUDE_DECK_DATA_DIR` だけでは防げない。
- `tmux -L cdverify` の別サーバーで動かす。`run.sh` は `TMUX` を引き継ぐので、アプリが作るセッション `cdverify` もこのサーバーに入る。`env -i` で `TMUX` を落とすと、利用者のふだんの tmux サーバーにセッションができる。
- `FD_DELAY`（既定 6 秒）は fd の 1 回の呼び出しごとに効く。探索 1 回は「リポジトリの数 + 1」回 fd を呼ぶので、既定の偽 HOME で約 40 秒かかる。探索中の振る舞いを見るための遅さ。
- 止め方は下の「止める」。`tmux kill-server` は使わない。

## 操作する

```bash
tmux -L cdverify send-keys -t run n
tmux -L cdverify send-keys -t run -l repos/claude
tmux -L cdverify send-keys -t run Down
tmux -L cdverify capture-pane -p -t run | sed -e 's#/tmp/cdv#$CDV#g' -e 's#/private$CDV#$CDV#g' | rg -v '^\s*$' | head -12
```

探索が終わるまで待つ。fd の呼び出しの間に一瞬だけ fd がいなくなるので、1 秒あけて 2 回見る。

```bash
until ! pgrep -f /tmp/cdv/bin/fd >/dev/null && sleep 1 && ! pgrep -f /tmp/cdv/bin/fd >/dev/null; do sleep 1; done
```

```bash
cut -c1-40 /tmp/cdv/fd.log
```

- **Esc と次のキーは、別の呼び出しで送る**。続けて送ると、端末が Alt+キーと解釈し、Esc が効かない。Esc を送った後は、画面が一覧に戻ったのを capture で確かめてから次を送る（反映まで 1 秒ほどかかる）。
- `send-keys -l` で `-` から始まる文字を送るときは `--` を挟む（`send-keys -t run -l -- -x`）。
- 偽 HOME のパスに `claude` と `deck` が含まれるので、`claude` だけのフィルタは全候補に当たる（あいまい一致）。絞り込みを見るなら `repos/claude` のように `repos/` から打つ。
- リポジトリ選択で Enter と C-Enter を押さない。セッションとワークスペースが作られる。
- 探索の失敗は `touch /tmp/cdv/fdfail` で起こす。戻すときは `rm /tmp/cdv/fdfail`。
- 候補を足す、消すには、`/tmp/cdv/home/repos/<名前>/.jj` を作る、消す。

## セッションを作って閉じる

`setup.sh` は、Claude Code の代わりに眠るだけの `bin/claude`、本物の jj を包む `bin/jj`、本物の jj リポジトリ `/tmp/cdv/jjrepo` を用意する。偽の claude は hook を動かさないので、ステータスは idle のまま変わらない。

`new` は tmux の中で打つ（`TMUX` が無いと、作るウィンドウの行き先が無い）。CLI 用の tmux セッションを分けて作り、そこへ送る。

```bash
tmux -L cdverify new-session -d -s cli -x 160 -y 40 "zsh -f"
tmux -L cdverify send-keys -t cli "/tmp/cdv/cli.sh new --dir /tmp/cdv/jjrepo > /tmp/cdv/new1.json; echo rc=\$?" Enter
jq -c . /tmp/cdv/new1.json
/tmp/cdv/cli.sh list
/tmp/cdv/cli.sh close <ID>
```

- `new` は数秒かかる。`tmux -L cdverify capture-pane -p -t cli` に `rc=0` が出てから JSON を読む。TUI の一覧に新しい行が出るのは、さらに次の更新（5 秒ごと）の後。
- `close` と `list` は tmux の外から打ってよい。
- ワークスペースは `/tmp/cdv/data/workspace/-tmp-cdv-jjrepo/<名前>` にできる。`jj workspace list` は `/tmp/cdv/jjrepo` で `HOME=/tmp/cdv/home jj workspace list --ignore-working-copy` と打つ。
- jj の作業コピーの取り込みを失敗させるには `touch /tmp/cdv/jjlogfail`（`jj log` だけが失敗する）。戻すときは `rm /tmp/cdv/jjlogfail`。呼ばれた jj は `/tmp/cdv/jj.log` に出る。
- `r` で再開するには、行に Claude Code のセッション ID が要る。偽の claude は書かないので、`sqlite3 /tmp/cdv/data/deck.db "update sessions set session_chain='[\"11111111-1111-4111-8111-111111111111\"]' where name='<名前>'"` で入れる。再開したウィンドウの画面に、偽の claude が引数と作業フォルダを出す。ID は行ごとに変える。同じ ID の行が 2 つあると、次に起動したとき片方が重複として store から消える。
- 行をまとめて作るときは、ワークスペースを作らない `new` を繰り返す。`tmux -L cdverify send-keys -t cli 'for i in $(seq 1 16); do /tmp/cdv/cli.sh new --dir /tmp/cdv/jjrepo --no-workspace > /dev/null; echo rc$i=$?; done; echo ALLDONE' Enter`
- `touch /tmp/cdv/slownew` で `new-window` が 4 秒遅れ、作成、再開、フォークの完了を待つ間に操作できる。戻すときは `rm /tmp/cdv/slownew`。
- TUI の `n` から作るには、偽 HOME の候補は `.jj` だけの空のフォルダなので、本物の jj リポジトリを置いてから探索を待つ。`mkdir -p /tmp/cdv/home/repos/realrepo` の後、`HOME=/tmp/cdv/home jj git init /tmp/cdv/home/repos/realrepo` を打つ。
- store の行は `sqlite3 /tmp/cdv/data/deck.db "select name,status,workspace_name,finished_at,last_activity from sessions"` で読める。時刻はナノ秒。TUI は終了時に、メモリの `last_activity` で store を上書きする（偽の claude では 0）。store の時刻を書き換えて並びを見るときは、CLI の `list` で見る。

## 一覧の表示を見る

- レート制限の行（ヘッダーの 2 行目）は、`/tmp/cdv/data/rate-limits.json` に `{"rate_limits":{"five_hour":{"used_percentage":42,"resets_at":1900000000},"seven_day":{"used_percentage":17,"resets_at":1900500000}}}` を書くと出る。起動中に書いても出る（fsnotify）。
- カーソルの行は、画面の 1 行目の名前と同じ名前の行で、`capture-pane -p -e` で背景 `48;5;237` が付く。`tmux -L cdverify capture-pane -p -e -t run | rg -n '48;5;237' | cut -c1-5` で行番号が出る。
- 高さは `tmux -L cdverify resize-window -t run -x 160 -y 16` で変える。
- 変更前の版と比べるときは、次の順に打ってビルドし、`run-old.sh` で起動する。`mkdir -p /tmp/cdv/old-src`、`git -C <リポジトリ> archive HEAD -o /tmp/cdv/old.tar`、`tar -x -C /tmp/cdv/old-src -f /tmp/cdv/old.tar`、`GOEXPERIMENT=jsonv2 go build -C /tmp/cdv/old-src -o /tmp/cdv/claude-deck-old ./cmd/claude-deck`、`sed 's#/claude-deck"#/claude-deck-old"#' /tmp/cdv/run.sh > /tmp/cdv/run-old.sh`、`chmod +x /tmp/cdv/run-old.sh`。CLI も古い版で打つなら、同じ `sed` で `cli.sh` から `cli-old.sh` を作る。store の列を足す変更は、古い版の `cli-old.sh new` と `close` で `deck.db` に行を作ってから、新しい版を起動して読ませる。

## 外部セッションを出す

偽 HOME の `~/.claude/projects` に JSONL を置くと、起動中の TUI が数秒で外部セッションとして一覧に出す。`cwd` には、claude-deck が作っていない jj ワークスペースを置くと、close で消さないことも見られる（`jj workspace add` で `/tmp/cdv/extws` を作る）。

```bash
mkdir -p /tmp/cdv/home/.claude/projects/-private-tmp-cdv-extws
printf '{"type":"user","cwd":"/private/tmp/cdv/extws","sessionId":"44444444-4444-4444-8444-444444444444","timestamp":"%s","message":{"role":"user","content":"external hello"}}\n' "$(date -u +%Y-%m-%dT%H:%M:%S.000Z)" > /tmp/cdv/home/.claude/projects/-private-tmp-cdv-extws/44444444-4444-4444-8444-444444444444.jsonl
```

- `cwd` の場所で claude-deck のセッションが動いていると、外部セッションとして出ない。`/tmp/cdv/jjrepo` は `new` の行と重なりやすいので避ける。
- 外部セッションで `r` を押すと store に取り込まれ、以後は claude-deck のセッションとして `x` で終了できる。

## 右ペインの切り替えを見る

右ペインは Ghostty の分割で、隔離環境には出ない。tmux セッション `cdverify` で選ばれているウィンドウが、右ペインに出るものだ。実行中のセッションはセッション ID の名前のウィンドウ、終了済みのセッションは `__preview__` と `/tmp/cdv/data/preview-selection` の `name`。

```bash
tmux -L cdverify display -p -t cdverify '#{window_name}'
jq -r .name /tmp/cdv/data/preview-selection
tmux -L cdverify capture-pane -p -t run | head -1
```

画面の 1 行目に、カーソルのあるセッションの名前が出る。`touch /tmp/cdv/slowtmux` で `select-window` が 0〜0.5 秒ずつ遅れ、完了順が入れ替わりやすくなる。j と k を 1 回の `send-keys` でまとめて送り（`tmux -L cdverify send-keys -t run k k j j j k`）、4 秒ほど待ってから上の 3 つを見る。

## つつく価値のある流れ

- 起動直後、探索が終わる前に `n` を押して文字を打つ。結果が届いても文字が残るか。
- `n` を押し、すぐ文字を打ち、カーソルを動かしてから、探索が終わるまで待つ。2 回目以降の `n` でも試す。探索の間に候補を足す、選んでいる候補を消す。
- 探索中に Esc と `n` を何回か繰り返す。`fd.log` の `-t d` の行（探索の始まり）が 1 回分しか増えないか。
- 探索中に Esc で閉じたまま、探索が終わるのを待つ。一覧の下に「リポジトリを更新中...」が残らないか。
- `fdfail` を置いて `n`。エラーが出て、前の候補と入力が残るか。
- 実行中と終了済みのセッションを混ぜ、`slowtmux` を置いて j/k をまとめて送る。選ばれたウィンドウと 1 行目の名前が食い違わないか。切り替えを並列に出す作りでは、6 回中 4 回食い違った。
- `jjlogfail` を置いて `close` と `x`。ワークスペースと中の未取り込みのファイルが残るか。外してからもう一度 `close` すると、取り込んでから消えるか。
- `[session] max_sessions` を行の数にして、最も古い終了済みの行がワークスペースを残しているときに `new` を打つ（prune が走る）。`jjlogfail` の有無で、ワークスペースが残るか消えるか。
- レート制限の行を出し、行を 15 件以上にして、高さ 15〜23 で `G` の後に k と j を 1 つずつ送る。毎回カーソルの行が描かれているか。窓の見積もりを View と別に持っていた版では、高さ 16 と 22 で描かれない位置があった。
- `slownew` を置いて終了済みの行で Enter を押し、完了前に j で動かすか、フィルタを変える。完了後にフィルタが解け、カーソル、1 行目、選ばれたウィンドウが再開した行になるか。
- `?` の直後や再開中に `C-c` を押し、3 秒以上待つ。「終了しますか? (y/n)」が残るか。
- store の `status` を `sqlite3` で知らない値（`bogus`）にする。落ちずに暗い印で出て、`x` で終了でき、`r` で再開できず、`claude-deck hook status idle` で上書きされないか。
- 外部セッションを `r` で取り込んでから `x` で閉じる。`cwd` の jj ワークスペースが残るか（`jj workspace list` と `jj.log` に `workspace forget` が無いこと）。
- `x` のステータス行は 5 秒ほどで消え、幅で切れる。`tmux -L cdverify resize-window -t run -x 60` で狭くして見る。

## 止める

```bash
tmux -L cdverify send-keys -t run C-c
tmux -L cdverify send-keys -t run y
tmux -L cdverify kill-session -t cdverify
tmux -L cdverify kill-session -t run
tmux -L cdverify kill-session -t cli
```

`cli` を作っていなければ、最後の行は「can't find session」で終わる。

`C-c` で「終了しますか? (y/n)」が出る。リポジトリ選択の画面にいるときは、先に Esc で一覧に戻る。
