#!/bin/sh
# verifier が残した確かめの台本を、準備から片付けまで 1 回で流し直す。
# 使い方: sh .claude/skills/verify/rerun.sh <作業フォルダ> <台本.sh>...
#   SOCK  tmux -L のソケットの名前。既定は cdverify-rerun（手で確かめるときの cdverify と分ける）
# 台本への約束: DIR（作業フォルダ）と SOCK を環境変数で渡し、TMUX をそのサーバーに向ける。
# 台本ごとに ok か FAIL を出し、出力は <台本>.out に書く。どれかが 0 以外なら終了コードは 1。
set -eu
[ $# -ge 2 ] || { echo "usage: rerun.sh <dir> <flow.sh>..." >&2; exit 2; }
DIR=$1
shift
SOCK=${SOCK:-cdverify-rerun}
# WHY 既定のサーバーを断る: 利用者のふだんの claude-deck のセッションに、台本の new や send が入る。
[ "$SOCK" != default ] || { echo "rerun.sh: SOCK=default is the user's tmux server" >&2; exit 2; }
# WHY 動いているサーバーを止めずに断る: 手で確かめている最中か、ほかの verifier が使っている。
if tmux -L "$SOCK" has-session 2>/dev/null; then
  echo "rerun.sh: a tmux server is running on -L $SOCK; stop it or pick another SOCK" >&2
  exit 2
fi
HERE=$(cd "$(dirname "$0")" && pwd -P)

# 前の回の状態を残さない: 失敗を起こす印、記録、外部セッションの JSONL、jj に登録したワークスペース。
if [ -d "$DIR" ]; then
  (cd "$DIR" && rm -rf data jjrepo claude-args home/.claude/projects fdfail jjlogfail slownew slowtmux fd.log jj.log select.log typed-*)
fi
sh "$HERE/setup.sh" "$DIR" >/dev/null
DIR=$(cd "$DIR" && pwd -P)

# 止めるのは、このスクリプトが起こした -L $SOCK のサーバーだけ（上で動いていないことを確かめてある）。
stop() { tmux -L "$SOCK" kill-server 2>/dev/null || true; }
trap stop EXIT
trap 'stop; exit 130' INT TERM
# サーバーを起こして生かしておくためのセッション。台本は TMUX でこのサーバーを指す。
"$DIR/tmux.sh" -L "$SOCK" new-session -d -s keep -x 160 -y 40 "zsh -f"
TMUX="$(tmux -L "$SOCK" display-message -p '#{socket_path}'),0,0"
export DIR SOCK TMUX

failed=0
for flow in "$@"; do
  out="${flow%.sh}.out"
  if sh "$flow" > "$out" 2>&1; then
    echo "ok   $flow → $out"
  else
    echo "FAIL $flow (exit $?) → $out"
    failed=1
  fi
done
exit "$failed"
