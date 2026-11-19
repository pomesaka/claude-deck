#!/bin/sh
# claude-deck を、本物のデータ、設定、~/.claude.json、tmux セッションから隔てて動かす環境を作る。
# 使い方: sh .claude/skills/verify/setup.sh <作業フォルダ>
# 作るもの:
#   <DIR>/claude-deck      作業コピーからビルドしたバイナリ（リポジトリの ./claude-deck は上書きしない）
#   <DIR>/home             偽の HOME。repos/ の下に .jj を持つリポジトリを置く。fd はここだけを探す
#   <DIR>/bin/fd           本物の fd を FD_DELAY 秒待ってから呼び、出力を逆順にする。呼ばれるたびに <DIR>/fd.log に 1 行書く。
#                          <DIR>/fdfail があると失敗する
#   <DIR>/xdg/claude-deck/config.toml  data_dir=<DIR>/data、tmux の session_name=cdverify
#   <DIR>/run.sh           隔てた環境変数で claude-deck を起動する。tmux の中で実行する
#   <DIR>/cli.sh           run.sh と同じ環境で CLI（new、close、list）を実行する。new は tmux の中で実行する
#   <DIR>/tmux.sh          隔てた環境で tmux を呼ぶ。tmux サーバーを起動する最初の new-session はこれで打つ
#   <DIR>/jjrepo           セッションを作れる本物の jj リポジトリ。HOME の外に置くので、n の探索には出ない
#   <DIR>/bin/claude       Claude Code の代わり。引数と作業フォルダを出して眠るだけ。hook は動かない。
#                          引数を 1 つずつ [ ] で囲んで <DIR>/claude-args/<時刻>-<pid>.txt に書く
#   <DIR>/bin/jj           本物の jj を呼ぶ。呼ばれるたびに <DIR>/jj.log に 1 行書く。<DIR>/jjlogfail があると jj log だけ失敗する
#   <DIR>/bin/tmux-slow    本物の tmux を呼ぶ。<DIR>/slowtmux があると select-window の前に 0〜0.5 秒待ち、<DIR>/select.log に書く。
#                          <DIR>/slownew があると new-window の前に 4 秒待つ（作成、再開、フォークが遅くなる）
set -eu
DIR=${1:?usage: setup.sh <dir>}
mkdir -p "$DIR"
DIR=$(cd "$DIR" && pwd -P)
REPO=$(cd "$(dirname "$0")/../../.." && pwd -P)
FD=$(command -v fd || echo "$HOME/.cargo/bin/fd")
TMUX_BIN=$(command -v tmux)
TMUX_BIN_DIR=$(dirname "$TMUX_BIN")
JJ=$(command -v jj)

mkdir -p "$DIR/home/repos" "$DIR/bin" "$DIR/xdg/claude-deck" "$DIR/data" "$DIR/xdgdata"
for r in zeta alpha claude-x mid claude-deck-y beta; do mkdir -p "$DIR/home/repos/$r/.jj"; done
mkdir -p "$DIR/home/repos/alpha/sub-claude"
touch "$DIR/home/repos/alpha/sub-claude/go.mod"

cat > "$DIR/bin/fd" <<EOF
#!/bin/sh
echo "\$(date +%T) start \$*" >> "$DIR/fd.log"
sleep "\${FD_DELAY:-6}"
if [ -e "$DIR/fdfail" ]; then echo "forced failure" >&2; exit 1; fi
"$FD" "\$@" | sort -r
EOF
chmod +x "$DIR/bin/fd"

cat > "$DIR/bin/claude" <<EOF
#!/bin/sh
mkdir -p "$DIR/claude-args"
for a in "\$@"; do printf '[%s]\n' "\$a"; done > "$DIR/claude-args/\$(date +%s)-\$\$.txt"
echo "fake claude args: \$*"
echo "pwd=\$(pwd)"
exec sleep 100000
EOF
cat > "$DIR/bin/jj" <<EOF
#!/bin/sh
echo "\$(date +%T) \$*" >> "$DIR/jj.log"
if [ "\$1" = log ] && [ -e "$DIR/jjlogfail" ]; then echo "Error: forced snapshot failure" >&2; exit 1; fi
exec "$JJ" "\$@"
EOF
cat > "$DIR/bin/tmux-slow" <<EOF
#!/bin/sh
if [ "\$1" = new-window ] && [ -e "$DIR/slownew" ]; then
  echo "\$(date +%T) slow \$*" >> "$DIR/select.log"
  sleep 4
fi
if [ "\$1" = select-window ] && [ -e "$DIR/slowtmux" ]; then
  perl -e 'select(undef,undef,undef,rand(0.5))'
  echo "\$(date +%T) \$*" >> "$DIR/select.log"
fi
exec "$TMUX_BIN" "\$@"
EOF
chmod +x "$DIR/bin/claude" "$DIR/bin/jj" "$DIR/bin/tmux-slow"

if [ ! -d "$DIR/jjrepo/.jj" ]; then
  mkdir -p "$DIR/jjrepo"
  (cd "$DIR/jjrepo" && HOME="$DIR/home" "$JJ" git init --quiet \
    && echo hello > a.txt \
    && HOME="$DIR/home" "$JJ" --config user.name=v --config user.email=v@example.com commit --quiet -m init)
fi

cat > "$DIR/xdg/claude-deck/config.toml" <<EOF
data_dir = "$DIR/data"
[tmux]
enabled = true
session_name = "cdverify"
command = "$DIR/bin/tmux-slow"
[discovery]
project_markers = ["go.mod"]
[commands]
claude = "$DIR/bin/claude"
jj = "$DIR/bin/jj"
EOF

# TMUX を引き継ぐので、アプリが作る tmux セッションは、run.sh を動かした tmux サーバーに入る。
cat > "$DIR/run.sh" <<EOF
#!/bin/sh
exec env -i HOME="$DIR/home" PATH="$DIR/bin:$TMUX_BIN_DIR:/usr/bin:/bin" TERM=xterm-256color TMUX="\$TMUX" \\
  XDG_CONFIG_HOME="$DIR/xdg" XDG_DATA_HOME="$DIR/xdgdata" FD_DELAY="\${FD_DELAY:-6}" "$DIR/claude-deck"
EOF
cat > "$DIR/cli.sh" <<EOF
#!/bin/sh
exec env -i HOME="$DIR/home" PATH="$DIR/bin:$TMUX_BIN_DIR:/usr/bin:/bin" TERM=xterm-256color TMUX="\$TMUX" \\
  XDG_CONFIG_HOME="$DIR/xdg" XDG_DATA_HOME="$DIR/xdgdata" "$DIR/claude-deck" "\$@"
EOF
# tmux サーバーは、起動したクライアントの環境を、アプリが new-window で作るウィンドウ（__preview__ など）に引き継ぐ。
# 本物の HOME と CLAUDE_DECK_DATA_DIR を持ち込むと、__preview__ が本物のデータディレクトリを読む。
cat > "$DIR/tmux.sh" <<EOF
#!/bin/sh
exec env -i HOME="$DIR/home" PATH="$DIR/bin:$TMUX_BIN_DIR:/usr/bin:/bin" TERM=xterm-256color \\
  XDG_CONFIG_HOME="$DIR/xdg" XDG_DATA_HOME="$DIR/xdgdata" "$TMUX_BIN" "\$@"
EOF
chmod +x "$DIR/run.sh" "$DIR/cli.sh" "$DIR/tmux.sh"

cd "$REPO"
GOEXPERIMENT=jsonv2 go build -o "$DIR/claude-deck" ./cmd/claude-deck
echo "ready: $DIR"
