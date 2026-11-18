---
name: verify
description: claude-deck の TUI を、本物のデータと tmux から隔てた環境で起動し、別の tmux サーバーの中でキーを送って画面を取り、変更が動くかを確かめる手順。
when_to_use: TUI（internal/tui）、リポジトリの探索、起動の流れを変えた後に、テストでなく実際に動かして確かめるとき。
---

# claude-deck を動かして確かめる

テストと型検査は、ここでは走らせない。証拠は `tmux capture-pane` で取った画面と、`fd.log` などの外から見える記録。

## 起動する

コマンドは 1 つずつ別の呼び出しで打てるよう、パスを書き切ってある。作業フォルダは `/tmp/cdv` にしている。並べて確かめるなら、フォルダとソケットの名前（`-L cdverify`）を担当ごとに変える。

```bash
sh .claude/skills/verify/setup.sh /tmp/cdv
tmux -L cdverify new-session -d -s run -x 160 -y 40 "/tmp/cdv/run.sh; sleep 600"
```

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

## つつく価値のある流れ

- 起動直後、探索が終わる前に `n` を押して文字を打つ。結果が届いても文字が残るか。
- `n` を押し、すぐ文字を打ち、カーソルを動かしてから、探索が終わるまで待つ。2 回目以降の `n` でも試す。探索の間に候補を足す、選んでいる候補を消す。
- 探索中に Esc と `n` を何回か繰り返す。`fd.log` の `-t d` の行（探索の始まり）が 1 回分しか増えないか。
- 探索中に Esc で閉じたまま、探索が終わるのを待つ。一覧の下に「リポジトリを更新中...」が残らないか。
- `fdfail` を置いて `n`。エラーが出て、前の候補と入力が残るか。

## 止める

```bash
tmux -L cdverify send-keys -t run C-c
tmux -L cdverify send-keys -t run y
tmux -L cdverify kill-session -t cdverify
tmux -L cdverify kill-session -t run
```

`C-c` で「終了しますか? (y/n)」が出る。リポジトリ選択の画面にいるときは、先に Esc で一覧に戻る。
