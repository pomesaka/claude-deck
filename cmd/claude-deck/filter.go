package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pomesaka/claude-deck/internal/session"
)

// sessionFilter is which sessions list and broadcast take. Every condition
// given must hold (AND); one left empty passes every session.
// WHY list と broadcast で 1 つにする: 同じ条件で list を流せば、送る前に宛先を確かめられる。
type sessionFilter struct {
	// Alias is text the alias must contain, ignoring case.
	Alias string
	// Repo is text the repository's name (the last element of repo_path) must contain, ignoring case.
	Repo string
	// Only are the sessions to take, each an ID or a name, exactly, as send --id takes them.
	Only []string
}

// register adds the flags that set f to fs: --alias, --repo and --only.
func (f *sessionFilter) register(fs *flag.FlagSet) {
	fs.StringVar(&f.Alias, "alias", "", "only sessions whose alias contains this text (case-insensitive)")
	fs.StringVar(&f.Repo, "repo", "", "only sessions whose repository name (the last element of repo_path) contains this text (case-insensitive)")
	fs.Func("only", "only these sessions: IDs or names, comma-separated, matched exactly", func(v string) error {
		for name := range strings.SplitSeq(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				f.Only = append(f.Only, name)
			}
		}
		return nil
	})
}

// matches reports whether s passes every condition of f.
func (f sessionFilter) matches(s session.Snapshot) bool {
	return aliasMatches(s.Alias, f.Alias) && repoMatches(s.RepoPath, f.Repo) && onlyMatches(s, f.Only)
}

// aliasMatches reports whether a session with the alias passes `--alias filter`.
// An empty filter passes every session; otherwise the alias must contain the
// filter, ignoring case, so a session without an alias never passes.
func aliasMatches(alias, filter string) bool {
	return filter == "" || strings.Contains(strings.ToLower(alias), strings.ToLower(filter))
}

// repoMatches reports whether a session in the repository repoPath passes
// `--repo filter`. An empty filter passes every session; otherwise the
// repository's name, the last element of repoPath, must contain the filter,
// ignoring case.
// WHY 末尾の名前だけ: パス全体だと、親ディレクトリ（github.com や利用者名）の語に
// どのリポジトリも当たってしまう。
func repoMatches(repoPath, filter string) bool {
	return filter == "" || strings.Contains(strings.ToLower(filepath.Base(repoPath)), strings.ToLower(filter))
}

// onlyMatches reports whether s is one of only. No names pass every session.
func onlyMatches(s session.Snapshot, only []string) bool {
	return len(only) == 0 || slices.ContainsFunc(only, func(n string) bool { return isNamed(s, n) })
}

// isNamed reports whether key names s: its ID or its name, exactly, as send --id
// (session.FindStored) takes it.
// WHY 部分一致とエイリアスを受けない: 打ち込む先を選ぶ条件なので、名前の一部が別のセッションに当たって
// 送られると取り返せない。エイリアスは send --id が受けないので、宛先の指し方を 1 通りにする。
func isNamed(s session.Snapshot, key string) bool {
	return key == string(s.ID) || key == s.Name
}

// checkOnly reports a name of only that names no session of targets, or
// more than one, the way send --id refuses it.
// WHY 送る前に止める: 打ち間違えた名前を除いて残りにだけ送ると、全部に送れたと取り違える。
func checkOnly(targets []session.Snapshot, only []string) error {
	for _, n := range only {
		count := 0
		for _, s := range targets {
			if isNamed(s, n) {
				count++
			}
		}
		switch {
		case count == 0:
			return fmt.Errorf("--only %s: no live session has this ID or name", n)
		case count > 1:
			return fmt.Errorf("--only %s: %d sessions have this name; use their IDs", n, count)
		}
	}
	return nil
}
