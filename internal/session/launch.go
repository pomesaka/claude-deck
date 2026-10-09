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

// LaunchKind describes how a session is being started.
type LaunchKind int

const (
	// LaunchNew creates a fresh session with a new Claude Code process.
	LaunchNew LaunchKind = iota
	// LaunchResume restarts a completed session using --resume.
	LaunchResume
	// LaunchFork creates a new session that forks an existing conversation.
	LaunchFork
)

func (k LaunchKind) String() string {
	switch k {
	case LaunchNew:
		return "New"
	case LaunchResume:
		return "Resume"
	case LaunchFork:
		return "Fork"
	default:
		return "Unknown"
	}
}

// LaunchIntent captures the user's intention when starting a session.
// Instead of three separate methods with overlapping logic (CreateSession,
// ResumeSession, ForkSession), callers construct a LaunchIntent and pass it
// to Manager.Launch. The Manager uses Kind to dispatch to the appropriate
// internal workflow while sharing common setup (watchProcess, persist, notify).
type LaunchIntent struct {
	Kind LaunchKind

	// RepoPath is the repository root (.jj parent). Required for New and Fork.
	RepoPath string
	// WorkingDir is the directory to run claude in (may differ from RepoPath for sub-projects).
	// Required for New, optional for Resume (falls back to session's stored path).
	WorkingDir string
	// WithWorkspace controls whether a jj workspace is created. Only used for New.
	WithWorkspace bool

	// SessionID is the deck session ID to resume. Required for Resume and Fork.
	SessionID DeckSessionID
}
