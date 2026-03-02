package session

import (
	"path/filepath"
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/usage"
)

// newExternalSession creates a Session from a usage.SessionInfo for an external (non-managed) session.
// info は usage.Reader の ListAllSessions / ReadSessionInfoByID が返すポインタ。
func newExternalSession(info *usage.SessionInfo) *Session { //nolint:unparam
	repoName := filepath.Base(info.CWD)
	sess := &Session{
		ID:              GenerateSessionID(),
		Name:            repoName,
		RepoPath:        info.CWD,
		RepoName:        repoName,
		WorkspacePath:   info.CWD,
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
