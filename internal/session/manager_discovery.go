package session

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/usage"
)

// jjRepoInfo holds resolved jj repository information for a directory.
type jjRepoInfo struct {
	// JJParent is the directory containing .jj (may be workspace or main repo).
	JJParent string
	// RepoRoot is the main repository root (resolved from workspace pointer if needed).
	RepoRoot string
	// IsWorkspace is true if .jj/repo is a file (workspace), false if directory (main repo).
	IsWorkspace bool
}

// resolveJJRepo walks up from dir looking for .jj/repo and returns jj repository info.
// Returns nil if no .jj directory is found.
func resolveJJRepo(dir string) *jjRepoInfo {
	cur := dir
	for {
		jjRepo := filepath.Join(cur, ".jj", "repo")
		fi, err := os.Lstat(jjRepo)
		if err == nil {
			if fi.IsDir() {
				// 本体リポジトリ: .jj/repo がディレクトリ
				return &jjRepoInfo{
					JJParent: cur,
					RepoRoot: cur,
				}
			}
			// ワークスペース: .jj/repo がファイル（中身は本体の .jj/repo への絶対パス）
			content, err := os.ReadFile(jjRepo)
			if err != nil {
				return nil
			}
			// ファイル内容は本体の .jj/repo ディレクトリへの絶対パス
			// filepath.Dir を2回適用して .jj/repo → .jj → リポルート
			mainRepoRoot := filepath.Dir(filepath.Dir(strings.TrimSpace(string(content))))
			return &jjRepoInfo{
				JJParent:    cur,
				RepoRoot:    mainRepoRoot,
				IsWorkspace: true,
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil
		}
		cur = parent
	}
}

// newExternalSession creates a Session from a usage.SessionInfo for an external (non-managed) session.
// info は usage.Reader の ListAllSessions / ReadSessionInfoByID が返すポインタ。
// jj ワークスペースの場合、Name にワークスペース名、RepoName に本体リポ名を設定して重複を防ぐ。
func newExternalSession(info *usage.SessionInfo) *Session { //nolint:unparam
	name, repoPath, repoName, subProjectDir := resolveExternalSessionPaths(info.CWD, info.SessionID)

	sess := &Session{
		ID:            GenerateSessionID(),
		Name:          name,
		RepoPath:      repoPath,
		RepoName:      repoName,
		WorkspacePath: info.CWD,
		SubProjectDir: subProjectDir,
		SessionChain:  []ClaudeSessionID{ClaudeSessionID(info.SessionID)},
		Status:        StatusUnmanaged,
		Prompt:          info.Prompt,
		PermissionMode:  info.PermissionMode,
		StartedAt:       info.StartedAt,
		LastActivity:    info.LastActivity,
		TokenUsage: TokenUsageFromStats(info.Tokens),
	}
	sess.rt.LogLines = make([]string, 0)
	// FinishedAt は「プロセスが終了した時刻」であり、「最後に JSONL が更新された時刻」ではない。
	// 外部セッションはプロセスが終了したかどうか不明（JSONL が止まっているだけかもしれない）。
	return sess
}

// resolveExternalSessionPaths determines name, repoPath, repoName, and subProjectDir
// from a CWD and session ID by checking for jj workspace structure.
func resolveExternalSessionPaths(cwd, sessionID string) (name, repoPath, repoName, subProjectDir string) {
	jjInfo := resolveJJRepo(cwd)

	switch {
	case jjInfo != nil && jjInfo.IsWorkspace:
		// jj ワークスペース: Name はワークスペースディレクトリ名、RepoName は本体リポ名
		name = filepath.Base(jjInfo.JJParent)
		repoPath = jjInfo.RepoRoot
		repoName = filepath.Base(jjInfo.RepoRoot)
		// CWD が jjParent より深い場合（ワークスペース内サブディレクトリ）
		if rel, err := filepath.Rel(jjInfo.JJParent, cwd); err == nil && rel != "." {
			subProjectDir = rel
		}

	case jjInfo != nil:
		// 本体リポジトリ内: Name はセッション ID 先頭8文字
		name = truncateSessionID(sessionID)
		repoPath = jjInfo.RepoRoot
		repoName = filepath.Base(jjInfo.RepoRoot)
		if rel, err := filepath.Rel(jjInfo.RepoRoot, cwd); err == nil && rel != "." {
			subProjectDir = rel
		}

	default:
		// jj なし
		name = truncateSessionID(sessionID)
		repoPath = cwd
		repoName = filepath.Base(cwd)
	}
	return
}

// truncateSessionID returns the first 8 characters of a session ID for display.
func truncateSessionID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// hasManagedSessionAtWorkspaceLocked returns true if any active (non-terminal,
// non-unmanaged) session has the given workspacePath.
// Caller must hold m.mu (at least for reading).
//
// SessionChain が空のまま hook 着火前に DiscoverExternalSessions が走ると
// 同じワークスペースの外部セッションが重複生成されるレースを防ぐ。
func (m *Manager) hasManagedSessionAtWorkspaceLocked(workspacePath string) bool {
	if workspacePath == "" {
		return false
	}
	for _, s := range m.sessions {
		// WorkspacePath は CreateSession 時に固定され変更されないため sess.mu 不要。
		if s.WorkspacePath != workspacePath {
			continue
		}
		// s.Status を s.mu なしで直読みする。concurrency.md の鉄則「m.mu 保持中に s.mu を
		// 取得しない」を守るための措置。Status の書き手は常に s.mu を保持し m.mu は保持しない
		// ため、読み値がナノ秒単位で古くなる可能性はあるが、この重複フィルタ用途では許容できる。
		status := s.Status
		if status != StatusUnmanaged && !status.IsTerminal() {
			return true
		}
	}
	return false
}

// knownClaudeSessionIDs returns a set of all Claude Code session IDs that are
// already tracked by the manager. This includes all chain entries (current and
// historical) for every tracked session, so /clear history is never re-imported.
func (m *Manager) knownClaudeSessionIDs() map[ClaudeSessionID]bool {
	sessions := m.copySessionsList() // releases m.mu before returning
	known := make(map[ClaudeSessionID]bool, len(sessions)*2)
	for _, s := range sessions {
		for _, id := range s.ChainIDs() { // acquires s.mu internally; m.mu not held here
			if id != "" {
				known[id] = true
			}
		}
	}
	return known
}

// hasClaudeSessionID returns true if any session in the chain contains claudeSessionID.
// REQUIRES m.mu to be held (at least for reading); accessing m.sessions without it is a data race.
// SessionChain is read directly (without s.mu) to avoid lock ordering violation (m.mu → s.mu
// is forbidden per concurrency.md). Writers of SessionChain always hold s.mu, never m.mu,
// so the concurrent read is a benign race acceptable for this deduplication check.
func (m *Manager) hasClaudeSessionID(claudeSessionID ClaudeSessionID) bool {
	for _, existing := range m.sessions {
		if slices.Contains(existing.SessionChain, claudeSessionID) {
			return true
		}
	}
	return false
}

// handleNewFile imports a newly discovered JSONL file as an external session
// if it is not already tracked.
func (m *Manager) handleNewFile(ev usage.FileEvent) {
	csID := ClaudeSessionID(ev.SessionID)
	if m.knownClaudeSessionIDs()[csID] {
		return
	}

	info := m.usage.ReadSessionInfoByID(ev.SessionID)
	if info == nil {
		return
	}

	sess := newExternalSession(info)

	m.mu.Lock()
	// Double-check: DiscoverExternalSessions との競合で重複を防ぐ
	if !m.hasClaudeSessionID(csID) && !m.hasManagedSessionAtWorkspaceLocked(info.CWD) {
		m.sessions[sess.ID] = sess
	}
	m.mu.Unlock()

	m.notifyChange()
}

// DiscoverExternalSessions scans Claude Code JSONL files and imports
// sessions not already tracked by claude-deck. These are marked External.
// Returns the number of added sessions and whether more sessions may be available.
// offset-based pagination: m.discoveryOffset を使って段階的に読み込む。
func (m *Manager) DiscoverExternalSessions() (added int, hasMore bool) {
	allInfos := m.usage.ListAllSessions(time.Duration(m.config.DiscoveryDays)*24*time.Hour, m.config.MaxSessions, m.discoveryOffset)

	known := m.knownClaudeSessionIDs()

	added = 0
	for _, info := range allInfos {
		csID := ClaudeSessionID(info.SessionID)
		if known[csID] {
			continue
		}

		sess := newExternalSession(info)

		m.mu.Lock()
		// Double-check: handleNewFile との競合で重複を防ぐ
		// さらに、hook 着火前（SessionChain 空）の managed セッションと同じワークスペースの
		// 外部セッションを誤って生成しないようワークスペースパスでもチェックする。
		if m.hasClaudeSessionID(csID) {
			debuglog.Printf("[discover] skipping duplicate session %s (known claude ID)", info.SessionID)
		} else if m.hasManagedSessionAtWorkspaceLocked(info.CWD) {
			debuglog.Printf("[discover] skipping session %s: active managed session at %s", info.SessionID, info.CWD)
		} else {
			m.sessions[sess.ID] = sess
			added++
		}
		m.mu.Unlock()
	}

	if added > 0 {
		m.notifyChange()
	}
	// 取得件数が limit に達したら続きがある可能性がある
	hasMore = len(allInfos) == m.config.MaxSessions
	return added, hasMore
}
