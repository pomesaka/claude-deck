package session

import "path/filepath"

// ResolveLaunchDir maps an arbitrary directory to the (repoPath, workingDir) pair that
// CreateSession expects. isJJ reports whether dir belongs to a jj repository.
//
// WHY ワークスペースを本体リポジトリへ解決する: CLI の呼び出し元（PM セッション）は自分の
// jj ワークスペース内にいることが多い。ワークスペースを repoPath として渡すと、新しい
// ワークスペースの作成先と、サブプロジェクトの相対パス（filepath.Rel(repoPath, workingDir)）が
// 本体リポジトリ基準でなくなる。ワークスペース内の相対位置は保ったまま本体側へ付け替える。
func ResolveLaunchDir(dir string) (repoPath, workingDir string, isJJ bool) {
	info := resolveJJRepo(dir)
	if info == nil {
		return dir, dir, false
	}
	rel, err := filepath.Rel(info.JJParent, dir)
	if err != nil {
		return info.RepoRoot, info.RepoRoot, true
	}
	return info.RepoRoot, filepath.Join(info.RepoRoot, rel), true
}
