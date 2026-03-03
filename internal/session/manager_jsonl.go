package session

import (
	"context"
	"sort"
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/usage"
)

// StartFileWatcher creates a MultiWatcher for JSONL files and starts it
// in a background goroutine. Write events are coalesced (2秒間隔) して
// LastActivity を更新。新規ファイルは 30 秒間隔の re-glob で発見する。
func (m *Manager) StartFileWatcher(ctx context.Context) error {
	mw, err := usage.NewMultiWatcher(m.usage.BaseDir(), 30*time.Second)
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
		csID := s.ClaudeSessionID
		s.mu.RUnlock()

		if csID == ev.SessionID {
			s.mu.Lock()
			old := s.LastActivity
			s.LastActivity = ev.ModTime
			s.mu.Unlock()
			debuglog.Printf("[filewrite] matched session %s (deck=%s) LastActivity %s -> %s", csID, s.ID, old.Format("15:04:05"), ev.ModTime.Format("15:04:05"))
			m.notifyChange()
			return
		}
	}
	debuglog.Printf("[filewrite] no matching session for %s", ev.SessionID)
}

// StreamSession starts JSONL streaming for the given session (detail pane selection).
// 前回のストリーミングがあれば停止し、新しいセッションのストリーミングを開始する。
// 同じセッションが既にストリーム中なら何もしない。
func (m *Manager) StreamSession(sessionID string) {
	m.mu.Lock()
	if m.activeStreamID == sessionID {
		m.mu.Unlock()
		return
	}
	// 前のストリームを停止
	if m.activeStreamCancel != nil {
		m.activeStreamCancel()
		m.activeStreamCancel = nil
		m.activeStreamID = ""
	}
	m.mu.Unlock()

	if sessionID == "" {
		return
	}

	m.mu.RLock()
	sess, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return
	}

	sess.mu.RLock()
	csID := sess.ClaudeSessionID
	prevCSID := sess.PreviousClaudeSessionID
	sess.mu.RUnlock()

	if csID == "" {
		return
	}

	path := m.usage.ResolveSessionPath(csID)
	if path == "" {
		return
	}

	// /clear 前の旧セッションのログエントリを先頭に連結するための prefix を取得
	var prefixEntries []usage.LogEntry
	if prevCSID != "" {
		if prevPath := m.usage.ResolveSessionPath(prevCSID); prevPath != "" {
			prev := usage.NewLogStreamer(prevPath)
			prev.ReadAll()
			prefixEntries = prev.Entries()
		}
	}

	ctx, cancel := context.WithCancel(m.ctx)
	m.mu.Lock()
	m.activeStreamID = sessionID
	m.activeStreamCancel = cancel
	m.mu.Unlock()

	onChange := func(entries []usage.LogEntry) {
		merged := entries
		if len(prefixEntries) > 0 {
			merged = make([]usage.LogEntry, 0, len(prefixEntries)+len(entries))
			merged = append(merged, prefixEntries...)
			merged = append(merged, entries...)
			// MaxEntries は末尾優先で cap
			if len(merged) > usage.MaxEntries {
				merged = merged[len(merged)-usage.MaxEntries:]
			}
		}
		sess.mu.Lock()
		sess.JSONLLogEntries = merged
		sess.mu.Unlock()
		m.notifyChange()
	}

	go func() {
		defer func() {
			m.mu.Lock()
			if m.activeStreamID == sessionID {
				m.activeStreamID = ""
				m.activeStreamCancel = nil
			}
			m.mu.Unlock()
		}()

		// Phase 1: 末尾読み込みで即座に表示
		s := usage.NewLogStreamer(path)
		fileSize := s.ReadTail(512 * 1024) // 512KB
		onChange(s.Entries())

		// Phase 2: fileSize 以降をストリーミング（新規書き込み検知）
		for {
			err := s.RunFrom(ctx, fileSize, onChange)
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				return
			}
			// エラー時は最初からやり直し
			s = usage.NewLogStreamer(path)
			fileSize = s.ReadTail(512 * 1024)
			onChange(s.Entries())
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
}

// stopActiveStream cancels the current streaming goroutine if it matches the given session.
func (m *Manager) stopActiveStream(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeStreamID == sessionID && m.activeStreamCancel != nil {
		m.activeStreamCancel()
		m.activeStreamCancel = nil
		m.activeStreamID = ""
	}
}

// HydrateFromJSONL reads Claude Code JSONL files and populates
// JSONL-derived fields for sessions.
// セッション数は DiscoverExternalSessions のページネーションで段階的に増えるため、
// 一度に hydrate する数は自然に制限される。ReadTokensByID は "usage" マーカー行のみ
// スキャンするため軽量。
func (m *Manager) HydrateFromJSONL() {
	sessions := m.copySessionsList()

	// 最近のセッションから hydrate（LastActivity → StartedAt の降順）
	sort.Slice(sessions, func(i, j int) bool {
		ti := sessions[i].sortTime()
		tj := sessions[j].sortTime()
		return ti.After(tj)
	})

	for _, sess := range sessions {
		m.hydrateSession(sess)
	}
}

// RefreshFromJSONL re-reads Claude Code JSONL files and updates all
// JSONL-derived fields (tokens, prompt, timestamps) for every session.
// Also discovers any new external sessions with offset-based pagination.
// 並行呼び出し時は前回の refresh が終わるまでスキップする。
func (m *Manager) RefreshFromJSONL() {
	if !m.refreshing.CompareAndSwap(false, true) {
		return
	}
	defer m.refreshing.Store(false)

	m.HydrateFromJSONL()

	_, hasMore := m.DiscoverExternalSessions()
	if hasMore {
		// 続きがある場合は offset を進めて次の tick で続きを読み込む
		m.discoveryOffset += m.config.MaxSessions
	} else {
		// 全件読み込み完了。先頭に戻して新規セッションの検知を継続する
		m.discoveryOffset = 0
	}
}

// hydrateSession updates token usage for a single session.
// メタデータ (prompt, timestamps 等) は Discover 時に取得済みなので、
// ここではトークン数だけを軽量スキャンで更新する。
func (m *Manager) hydrateSession(sess *Session) {
	sess.mu.RLock()
	csID := sess.ClaudeSessionID
	sess.mu.RUnlock()

	if csID == "" {
		return
	}

	tokens := m.usage.ReadTokensByID(csID)
	if tokens == nil {
		return
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.TokenUsage = TokenUsage{
		InputTokens:              tokens.InputTokens,
		OutputTokens:             tokens.OutputTokens,
		CacheCreationInputTokens: tokens.CacheCreationInputTokens,
		CacheReadInputTokens:     tokens.CacheReadInputTokens,
		EstimatedCostUSD:         tokens.EstimatedCostUSD,
	}
}
