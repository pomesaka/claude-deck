package session

import (
	"context"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/hooks"
)

// StartEventWatcher watches the hook events file and updates ClaudeSessionID
// when /clear or compact causes Claude Code to issue a new session ID.
// SessionEnd→SessionStart ペアリングにより旧 ID → 新 ID の紐付けを行う。
func (m *Manager) StartEventWatcher(ctx context.Context) error {
	eventsPath := hooks.EventsFilePath(m.config.DataDir)

	// 起動時に古いイベントを破棄（新規イベントのみ処理）
	if err := hooks.TruncateEventsFile(eventsPath); err != nil {
		debuglog.Printf("[event-watcher] truncate failed: %v", err)
	}

	return hooks.WatchEvents(ctx, eventsPath, func(ev hooks.Event) {
		m.handleHookEvent(ev)
	})
}

// handleHookEvent processes hook events using SessionEnd→SessionStart pairing.
//
// /clear や compact では以下の順序でイベントが発火する:
//  1. SessionEnd  {session_id: OLD, reason: "clear"}
//  2. SessionStart {session_id: NEW, source: "clear"}
//
// SessionEnd の session_id (OLD) で managed セッションを特定し、
// SessionStart の session_id (NEW) に ClaudeSessionID を更新する。
// CWD ではなく ID ベースでマッチングするため、同一ディレクトリの
// 複数インスタンスが混同されない。
func (m *Manager) handleHookEvent(ev hooks.Event) {
	switch ev.HookEventName {
	case hooks.EventNotification:
		sess := m.findSessionByClaudeID(ev.SessionID)
		if sess == nil {
			debuglog.Printf("[event-watcher] Notification: no managed session for %s", ev.SessionID)
			return
		}
		switch ev.NotificationType {
		case hooks.NotifyPermissionPrompt:
			sess.SetStatus(StatusWaitingApproval)
		case hooks.NotifyElicitationDialog:
			sess.SetStatus(StatusWaitingAnswer)
		case hooks.NotifyIdlePrompt:
			sess.SetStatus(StatusIdle)
		default:
			debuglog.Printf("[event-watcher] Notification: unknown type %q", ev.NotificationType)
			return
		}
		debuglog.Printf("[event-watcher] Notification: session=%s type=%s → %s",
			ev.SessionID, ev.NotificationType, sess.GetStatus())
		m.notifyChange()

	case hooks.EventStop:
		sess := m.findSessionByClaudeID(ev.SessionID)
		if sess == nil {
			debuglog.Printf("[event-watcher] Stop: no managed session for %s", ev.SessionID)
			return
		}
		sess.SetStatus(StatusIdle)
		debuglog.Printf("[event-watcher] Stop: session=%s → StatusIdle", ev.SessionID)
		m.notifyChange()

	case hooks.EventSessionEnd:
		if ev.ClaudeDeckSessionID == "" {
			debuglog.Printf("[event-watcher] SessionEnd: no ClaudeDeckSessionID, skipping pairing (session_id=%s)", ev.SessionID)
			return
		}
		// hookProc は event watcher goroutine のみがアクセスするため mu 不要
		m.hookProc.storePending(ev.ClaudeDeckSessionID, &ev)

	case hooks.EventSessionStart:
		debuglog.Printf("[event-watcher] SessionStart: session_id=%s source=%s claude_deck_session_id=%s",
			ev.SessionID, ev.Source, ev.ClaudeDeckSessionID)

		// startup/resume: 環境変数で渡した ClaudeDeckSessionID でセッションを特定し、
		// Claude Code が割り当てた session_id を紐付ける
		if ev.Source == hooks.SourceStartup || ev.Source == hooks.SourceResume {
			if ev.ClaudeDeckSessionID == "" {
				debuglog.Printf("[event-watcher] SessionStart source=%s but no ClaudeDeckSessionID, skipping", ev.Source)
				return
			}
			m.mu.RLock()
			sess := m.sessions[ev.ClaudeDeckSessionID]
			m.mu.RUnlock()
			if sess == nil {
				debuglog.Printf("[event-watcher] SessionStart: no session for ClaudeDeckSessionID=%s", ev.ClaudeDeckSessionID)
				return
			}
			sess.mu.Lock()
			curID := sess.CurrentClaudeID()
			if curID != "" {
				sess.mu.Unlock()
				debuglog.Printf("[event-watcher] SessionStart source=%s skipped: session %s already has ClaudeSessionID=%s",
					ev.Source, ev.ClaudeDeckSessionID, curID)
				return
			}
			sess.appendToChainLocked(ev.SessionID)
			sess.mu.Unlock()
			debuglog.Printf("[event-watcher] session %s: ClaudeSessionID set to %s (source=%s)",
				ev.ClaudeDeckSessionID, ev.SessionID, ev.Source)
			m.persist(sess)
			m.notifyChange()
			return
		}

		// source が clear/compact でなければ更新不要
		if ev.Source != hooks.SourceClear && ev.Source != hooks.SourceCompact {
			return
		}

		// ペアリング: hookProc から対応する SessionEnd を取り出す（mu 不要）
		var pendEnd *hooks.Event
		if ev.ClaudeDeckSessionID != "" {
			pendEnd = m.hookProc.consumePending(ev.ClaudeDeckSessionID)
		}

		if pendEnd == nil {
			debuglog.Printf("[event-watcher] no pending SessionEnd for source=%s deck_session=%s, skipping", ev.Source, ev.ClaudeDeckSessionID)
			return
		}

		oldCSID := pendEnd.SessionID
		newCSID := ev.SessionID
		if oldCSID == newCSID {
			return
		}

		// ClaudeDeckSessionID で managed セッションを直接特定
		m.mu.RLock()
		sess := m.sessions[ev.ClaudeDeckSessionID]
		m.mu.RUnlock()
		if sess == nil {
			debuglog.Printf("[event-watcher] no managed session for ClaudeDeckSessionID %s", ev.ClaudeDeckSessionID)
			return
		}

		sessionID := ev.ClaudeDeckSessionID

		debuglog.Printf("[event-watcher] session %s: ClaudeSessionID %s → %s (source=%s)",
			sessionID, oldCSID, newCSID, ev.Source)

		sess.mu.Lock()
		// SessionChain に新 ID を追記（旧 ID は chain 内に残り knownClaudeSessionIDs で参照される）
		sess.appendToChainLocked(newCSID)
		sess.mu.Unlock()
		// /clear 時はログをリセット（新セッションのログのみ表示）
		// rt.mu と sess.mu を同時保持しないためロックを分ける
		sess.rt.mu.Lock()
		sess.rt.JSONLLogEntries = nil
		sess.rt.mu.Unlock()

		m.persist(sess)

		// JSONL ストリーミングを新セッションに切り替え
		m.mu.RLock()
		activeID := m.activeStreamID
		m.mu.RUnlock()
		if activeID == sessionID {
			m.stopActiveStream(sessionID)
			m.StreamSession(sessionID)
		}

		m.notifyChange()
	}
}

// findSessionByClaudeID returns the managed session (with an active process)
// matching the given Claude Code session ID, or nil if not found.
func (m *Manager) findSessionByClaudeID(claudeSessionID string) *Session {
	// m.mu と s.mu を同時に保持しない（ABBA 回避）
	m.mu.RLock()
	var candidates []*Session
	for id, proc := range m.processes {
		select {
		case <-proc.Done():
			continue
		default:
		}
		if s, ok := m.sessions[id]; ok {
			candidates = append(candidates, s)
		}
	}
	m.mu.RUnlock()

	for _, s := range candidates {
		s.mu.RLock()
		csID := s.CurrentClaudeID()
		s.mu.RUnlock()
		if csID == claudeSessionID {
			return s
		}
	}
	return nil
}
