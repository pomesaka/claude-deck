package session

import (
	"os"
	"path/filepath"
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
		ID:              GenerateSessionID(),
		Name:            name,
		RepoPath:        repoPath,
		RepoName:        repoName,
		WorkspacePath:   info.CWD,
		SubProjectDir:   subProjectDir,
		ClaudeSessionID: info.SessionID,
		Status:          StatusUnmanaged,
		Prompt:          info.Prompt,
		PermissionMode:  info.PermissionMode,
		StartedAt:       info.StartedAt,
		LastActivity:    info.LastActivity,
		TokenUsage: TokenUsage{
			InputTokens:              info.Tokens.InputTokens,
			OutputTokens:             info.Tokens.OutputTokens,
			CacheCreationInputTokens: info.Tokens.CacheCreationInputTokens,
			CacheReadInputTokens:     info.Tokens.CacheReadInputTokens,
			EstimatedCostUSD:         info.Tokens.EstimatedCostUSD,
		},
		LogLines: make([]string, 0),
	}
	if !info.LastActivity.IsZero() {
		t := info.LastActivity
		sess.FinishedAt = &t
	}
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

// knownClaudeSessionIDs returns a set of all Claude Code session IDs that are
// already tracked by the manager. This includes current IDs, previous IDs
// (from /clear), and old IDs from deleted sessions.
func (m *Manager) knownClaudeSessionIDs() map[string]bool {
	sessions := m.copySessionsList()
	known := make(map[string]bool, len(sessions)*2)
	for _, s := range sessions {
		s.mu.RLock()
		if s.ClaudeSessionID != "" {
			known[s.ClaudeSessionID] = true
		}
		if s.PreviousClaudeSessionID != "" {
			known[s.PreviousClaudeSessionID] = true
		}
		s.mu.RUnlock()
	}
	m.mu.RLock()
	for id := range m.oldSessionIDs {
		known[id] = true
	}
	m.mu.RUnlock()
	return known
}

// handleNewFile imports a newly discovered JSONL file as an external session
// if it is not already tracked.
func (m *Manager) handleNewFile(ev usage.FileEvent) {
	if m.knownClaudeSessionIDs()[ev.SessionID] {
		return
	}

	info := m.usage.ReadSessionInfoByID(ev.SessionID)
	if info == nil {
		return
	}

	sess := newExternalSession(info)

	m.mu.Lock()
	// Double-check: DiscoverExternalSessions との競合で重複を防ぐ
	for _, existing := range m.sessions {
		if existing.ClaudeSessionID == ev.SessionID {
			m.mu.Unlock()
			return
		}
	}
	m.sessions[sess.ID] = sess
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
		if known[info.SessionID] {
			continue
		}

		sess := newExternalSession(info)

		m.mu.Lock()
		// Double-check: handleNewFile との競合で重複を防ぐ
		duplicate := false
		for _, existing := range m.sessions {
			if existing.ClaudeSessionID == info.SessionID {
				duplicate = true
				break
			}
		}
		if !duplicate {
			m.sessions[sess.ID] = sess
			added++
		} else {
			debuglog.Printf("[discover] skipping duplicate session %s", info.SessionID)
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
