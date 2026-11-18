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
set -eu
DIR=${1:?usage: setup.sh <dir>}
mkdir -p "$DIR"
DIR=$(cd "$DIR" && pwd -P)
REPO=$(cd "$(dirname "$0")/../../.." && pwd -P)
FD=$(command -v fd || echo "$HOME/.cargo/bin/fd")
TMUX_BIN_DIR=$(dirname "$(command -v tmux)")

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

cat > "$DIR/xdg/claude-deck/config.toml" <<EOF
data_dir = "$DIR/data"
[tmux]
enabled = true
session_name = "cdverify"
[discovery]
project_markers = ["go.mod"]
EOF

# TMUX を引き継ぐので、アプリが作る tmux セッションは、run.sh を動かした tmux サーバーに入る。
cat > "$DIR/run.sh" <<EOF
#!/bin/sh
exec env -i HOME="$DIR/home" PATH="$DIR/bin:$TMUX_BIN_DIR:/usr/bin:/bin" TERM=xterm-256color TMUX="\$TMUX" \\
  XDG_CONFIG_HOME="$DIR/xdg" XDG_DATA_HOME="$DIR/xdgdata" FD_DELAY="\${FD_DELAY:-6}" "$DIR/claude-deck"
EOF
chmod +x "$DIR/run.sh"

cd "$REPO"
GOEXPERIMENT=jsonv2 go build -o "$DIR/claude-deck" ./cmd/claude-deck
echo "ready: $DIR"
