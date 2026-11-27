package jj

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
)

// lockWait is how long a jj command waits for another claude-deck jj command
// on the same repository before it runs anyway.
// WHY 待ちきったら走らせる: 前の jj が jj git fetch のネットワーク待ちで止まると、
// close や new がいつまでも終わらなくなる。走らせれば分岐はありうるが、
// 戻らないよりはよい。
var lockWait = 2 * time.Minute

// lockRepo takes the lock that lets one claude-deck jj command at a time write
// to the repository dir belongs to (its root or any of its workspaces), and
// returns the function that releases it.
//
// WHY 直列にする: 同じリポジトリの jj を並べて走らせると操作ログが分岐し、jj は次の
// 実行で分岐を merge する（reconcile divergent operations）。jj 0.37 は merge の後の
// 操作ログで作業コピーの操作を祖先と見つけられないことがあり（dag_walk の
// closest_common_node が両側から同じ歩数で辿るため）、その作業コピーで jj が
// 「sibling of the working copy's operation」で止まる。close や new を続けて、
// 別々のプロセスから実行したときに起きた（jj-vcs/jj#9314、v0.41.0 で直った）。
// WHY ロックはリポジトリの store のディレクトリそのもの: ワークスペースがどれも
// 共有する唯一の場所で、ファイルを足さずに済む。flock は open したファイルごとの
// ロックなので、別のプロセスとも、同じプロセスの別の goroutine とも排他になる。
// 排他は claude-deck の jj の間だけで、エージェントが自分で打つ jj とは並びうる。
func lockRepo(dir string) (unlock func()) {
	storeDir, ok := repoStoreDir(dir)
	if !ok {
		return func() {}
	}
	f, err := os.Open(storeDir)
	if err != nil {
		debuglog.Printf("[jj.lockRepo] opening %s: %v, running without the lock", storeDir, err)
		return func() {}
	}
	deadline := time.Now().Add(lockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			debuglog.Printf("[jj.lockRepo] locking %s: %v, running without the lock", storeDir, err)
			f.Close()
			return func() {}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// repoStoreDir returns the .jj/repo directory of the repository dir belongs
// to. In a workspace other than the default one, .jj/repo is a file holding
// that directory's path.
func repoStoreDir(dir string) (string, bool) {
	for d := dir; ; d = filepath.Dir(d) {
		p := filepath.Join(d, ".jj", "repo")
		info, err := os.Stat(p)
		switch {
		case err == nil && info.IsDir():
			return p, true
		case err == nil:
			data, err := os.ReadFile(p)
			if err != nil {
				return "", false
			}
			target := strings.TrimSpace(string(data))
			if !filepath.IsAbs(target) {
				target = filepath.Join(d, ".jj", target)
			}
			return target, true
		}
		if parent := filepath.Dir(d); parent == d {
			return "", false
		}
	}
}
