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
		sess := m.findSessionByClaudeID(ClaudeSessionID(ev.SessionID))
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
		m.notifyChange(sess.ID)

	case hooks.EventUserPromptSubmit:
		m.handleStatusEvent(ClaudeSessionID(ev.SessionID), "UserPromptSubmit", StatusRunning)

	case hooks.EventPreToolUse:
		// ツール実行開始 = Claude が処理中。WaitingAnswer/WaitingApproval からの復帰も含む。
		// ask_followup_question 自体も PreToolUse を発火するが、直後の Notification
		// (elicitation_dialog) が WaitingAnswer に上書きするため問題ない。
		m.handleStatusEvent(ClaudeSessionID(ev.SessionID), "PreToolUse", StatusRunning)

	case hooks.EventStop:
		m.handleStatusEvent(ClaudeSessionID(ev.SessionID), "Stop", StatusIdle)

	case hooks.EventPostToolUseFailure:
		// ユーザーが Esc 等でツール実行を中断した場合。プロセスは生きているので Idle に戻す。
		m.handleStatusEvent(ClaudeSessionID(ev.SessionID), "PostToolUseFailure", StatusIdle)

	case hooks.EventStopFailure:
		// API エラーでターンが失敗した場合。プロセスは生きているので Idle に戻す。
		m.handleStatusEvent(ClaudeSessionID(ev.SessionID), "StopFailure", StatusIdle)

	case hooks.EventSessionEnd:
		if ev.ClaudeDeckSessionID == "" {
			debuglog.Printf("[event-watcher] SessionEnd: no ClaudeDeckSessionID, skipping pairing (session_id=%s)", ev.SessionID)
			return
		}
		// hookProc は event watcher goroutine のみがアクセスするため mu 不要
		m.hookProc.storePending(DeckSessionID(ev.ClaudeDeckSessionID), &ev)

	case hooks.EventSessionStart:
		debuglog.Printf("[event-watcher] SessionStart: session_id=%s source=%s claude_deck_session_id=%s",
			ev.SessionID, ev.Source, ev.ClaudeDeckSessionID)

		// Boundary conversion: hooks.Event の string → typed IDs
		deckID := DeckSessionID(ev.ClaudeDeckSessionID)
		claudeID := ClaudeSessionID(ev.SessionID)

		// startup/resume: 環境変数で渡した ClaudeDeckSessionID でセッションを特定し、
		// Claude Code が割り当てた session_id を紐付ける
		if ev.Source == hooks.SourceStartup || ev.Source == hooks.SourceResume {
			if deckID == "" {
				debuglog.Printf("[event-watcher] SessionStart source=%s but no ClaudeDeckSessionID, skipping", ev.Source)
				return
			}
			m.mu.RLock()
			sess := m.sessions[deckID]
			m.mu.RUnlock()
			if sess == nil {
				debuglog.Printf("[event-watcher] SessionStart: no session for ClaudeDeckSessionID=%s", deckID)
				return
			}
			sess.mu.Lock()
			curID := sess.CurrentClaudeID()
			if curID != "" {
				sess.mu.Unlock()
				debuglog.Printf("[event-watcher] SessionStart source=%s skipped: session %s already has ClaudeSessionID=%s",
					ev.Source, deckID, curID)
				return
			}
			sess.appendToChainLocked(claudeID)
			sess.mu.Unlock()
			debuglog.Printf("[event-watcher] session %s: ClaudeSessionID set to %s (source=%s)",
				deckID, claudeID, ev.Source)
			m.persist(sess)
			m.notifyChange(deckID)
			return
		}

		// source が clear/compact でなければ更新不要
		if ev.Source != hooks.SourceClear && ev.Source != hooks.SourceCompact {
			return
		}
		if deckID == "" {
			debuglog.Printf("[event-watcher] SessionStart source=%s but no ClaudeDeckSessionID, skipping", ev.Source)
			return
		}

		// ペアリング: hookProc から対応する SessionEnd を取り出す（mu 不要）
		pendEnd := m.hookProc.consumePending(deckID)

		if pendEnd == nil {
			debuglog.Printf("[event-watcher] no pending SessionEnd for source=%s deck_session=%s, skipping", ev.Source, deckID)
			return
		}

		oldCSID := pendEnd.SessionID
		newCSID := ev.SessionID
		if oldCSID == newCSID {
			return
		}

		// ClaudeDeckSessionID で managed セッションを直接特定
		m.mu.RLock()
		sess := m.sessions[deckID]
		m.mu.RUnlock()
		if sess == nil {
			debuglog.Printf("[event-watcher] no managed session for ClaudeDeckSessionID %s", deckID)
			return
		}

		debuglog.Printf("[event-watcher] session %s: ClaudeSessionID %s → %s (source=%s)",
			deckID, oldCSID, newCSID, ev.Source)

		sess.mu.Lock()
		// SessionChain に新 ID を追記（旧 ID は chain 内に残り knownClaudeSessionIDs で参照される）
		sess.appendToChainLocked(claudeID)
		sess.mu.Unlock()
		// /clear 時はログをリセット（新セッションのログのみ表示）
		// rt.mu と sess.mu を同時保持しないためロックを分ける
		sess.rt.mu.Lock()
		sess.rt.JSONLLogEntries = nil
		sess.rt.mu.Unlock()

		m.persist(sess)

		// JSONL ストリーミングを新セッションに切り替え
		if m.stream.isCurrent(deckID) {
			m.stopActiveStream(deckID)
			m.StreamSession(deckID)
		}

		m.notifyChange(deckID)
	}
}

// findSessionByClaudeID returns the managed session (with an active process)
// matching the given Claude Code session ID, or nil if not found.
//
// Uses session.process.Load() != nil to determine liveness — the same domain
// concept used by DisplayChannel derivation. This avoids O(N) tmux list-windows
// calls (one per session) that backend.IsActive would require.
// Hook events are only sent by running processes, so any race window between
// process death and DetachProcess() is the same as before.
func (m *Manager) findSessionByClaudeID(claudeSessionID ClaudeSessionID) *Session {
	candidates := m.copySessionsList()

	for _, s := range candidates {
		if !s.IsProcessAlive() {
			continue
		}
		s.mu.RLock()
		csID := s.CurrentClaudeID()
		s.mu.RUnlock()
		if csID == claudeSessionID {
			return s
		}
	}
	return nil
}

// handleStatusEvent is the common handler for hook events that simply transition
// a session's status. It finds the session by Claude session ID, sets the new
// status, logs the transition, and fires a change notification.
func (m *Manager) handleStatusEvent(claudeID ClaudeSessionID, eventName string, status Status) {
	sess := m.findSessionByClaudeID(claudeID)
	if sess == nil {
		debuglog.Printf("[event-watcher] %s: no managed session for %s", eventName, claudeID)
		return
	}
	sess.SetStatus(status)
	debuglog.Printf("[event-watcher] %s: session=%s → %s", eventName, claudeID, status)
	m.notifyChange(sess.ID)
}
