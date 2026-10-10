package session

import (
	"context"
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/ratelimits"
	"github.com/pomesaka/claude-deck/internal/usage"
)

// StartFileWatcher creates a MultiWatcher for JSONL files and starts it
// in a background goroutine. Write events are coalesced (2秒間隔) して
// LastActivity を更新。新規ファイルは 30 秒間隔の re-glob で発見する。
func (m *Manager) StartFileWatcher(ctx context.Context) error {
	mw, err := m.usage.NewMultiWatcher(30 * time.Second)
	if err != nil {
		return err
	}

	mw.OnWrite = m.handleFileWrite
	mw.OnNewFile = m.handleNewFile

	m.mu.Lock()
	m.fileWatcher = mw
	m.mu.Unlock()

	go mw.Run(ctx)
	return nil
}

// handleFileWrite updates LastActivity for the session matching the written JSONL file.
// ロック順序: m.mu を先に解放してから s.mu を取る（ABBA 回避）。
func (m *Manager) handleFileWrite(ev usage.FileEvent) {
	sessions := m.copySessionsList()
	debuglog.Printf("[filewrite] ev.SessionID=%s modTime=%s sessions=%d", ev.SessionID, ev.ModTime.Format("15:04:05"), len(sessions))
	for _, s := range sessions {
		s.mu.RLock()
		csID := s.CurrentRuntimeID()
		s.mu.RUnlock()

		if string(csID) == ev.SessionID {
			s.ApplyFileActivity(ev.ModTime)
			m.applyRuntimeActivityFromJSONL(s, ev)
			debuglog.Printf("[filewrite] matched session %s (deck=%s) LastActivity -> %s", csID, s.ID, ev.ModTime.Format("15:04:05"))
			m.notifyChange(s.ID)
			return
		}
	}
	debuglog.Printf("[filewrite] no matching session for %s", ev.SessionID)
}

func (m *Manager) applyRuntimeActivityFromJSONL(sess *Session, ev usage.FileEvent) {
	activity := m.usage.ReadRuntimeActivity(ev.Path)
	if activity.SessionID != "" && activity.SessionID != ev.SessionID {
		return
	}
	if activity.RateLimits != nil {
		m.applyRuntimeRateLimits(activity.RateLimits)
	}
	if activity.Kind == usage.RuntimeActivityNone && activity.CurrentTool == "" && !activity.ClearTool {
		return
	}
	if !sess.IsProcessAlive() {
		return
	}

	switch activity.Kind {
	case usage.RuntimeActivityRunning:
		m.recordRuntimeStatus(sess, StatusRunning)
	case usage.RuntimeActivityIdle:
		m.recordRuntimeStatus(sess, StatusIdle)
	}
	if activity.CurrentTool != "" {
		sess.SetCurrentTool(activity.CurrentTool)
	} else if activity.ClearTool {
		sess.SetCurrentTool("")
	}
}

// recordRuntimeStatus writes a status read from the runtime's JSONL to the store.
// Runtimes without hooks (Codex) report their status this way; the store stays the
// only place the status is decided, as with `claude-deck hook status`.
func (m *Manager) recordRuntimeStatus(sess *Session, status Status) {
	if sess.GetStatus() == status {
		return
	}
	if err := RecordHookStatus(m.store, sess.ID, status); err != nil {
		debuglog.Printf("[recordRuntimeStatus] %s: %v", sess.ID, err)
		return
	}
	m.Reload()
}

func (m *Manager) applyRuntimeRateLimits(limits *usage.RuntimeRateLimits) {
	if limits == nil {
		return
	}
	var status ratelimits.Status
	if limits.FiveHourAvailable {
		status.FiveHour = ratelimits.Window{
			UsedPct:  limits.FiveHour.UsedPct,
			ResetsAt: time.Unix(limits.FiveHour.ResetsAt, 0),
		}
		status.FiveHourAvailable = true
	}
	if limits.SevenDayAvailable {
		status.SevenDay = ratelimits.Window{
			UsedPct:  limits.SevenDay.UsedPct,
			ResetsAt: time.Unix(limits.SevenDay.ResetsAt, 0),
		}
		status.SevenDayAvailable = true
	}
	if !status.FiveHourAvailable && !status.SevenDayAvailable {
		return
	}
	if err := ratelimits.Save(m.config.DataDir, status); err != nil {
		debuglog.Printf("[ratelimits] save failed: %v", err)
	}
}

// RefreshFromJSONL is the periodic refresh: it marks sessions whose window is
// gone as exited, re-reads the jj bookmarks, and discovers new external
// sessions with offset-based pagination.
// 並行呼び出し時は前回の refresh が終わるまでスキップする。
func (m *Manager) RefreshFromJSONL() {
	if !m.refreshing.CompareAndSwap(false, true) {
		return
	}
	defer m.refreshing.Store(false)

	m.markVanishedSessions()
	m.refreshBookmarks()

	_, hasMore := m.DiscoverExternalSessions()
	if hasMore {
		// 続きがある場合は offset を進めて次の tick で続きを読み込む
		m.discoveryOffset += m.config.MaxSessions
	} else {
		// 全件読み込み完了。先頭に戻して新規セッションの検知を継続する
		m.discoveryOffset = 0
	}
}

// refreshBookmarks updates BookmarkName for active sessions with workspaces.
// 完了/エラーのセッションはブックマークが変わらないためスキップする。
func (m *Manager) refreshBookmarks() {
	for _, sess := range m.copySessionsList() {
		sess.mu.RLock()
		wsPath := sess.WorkspacePath
		status := sess.Status
		sess.mu.RUnlock()

		if wsPath == "" {
			continue
		}
		if status == StatusCompleted || status == StatusError {
			continue
		}

		bookmark, err := m.jj().GetNearestBookmark(wsPath)
		if err != nil {
			debuglog.Printf("[refreshBookmarks] session %s: %v", sess.ID, err)
			continue
		}
		if bookmark == "" {
			continue
		}

		sess.ApplyBookmark(bookmark)
	}
}
