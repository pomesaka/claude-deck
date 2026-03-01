package session

import (
	json "encoding/json/v2"
	"encoding/json/jsontext"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/pomesaka/sandbox/claude-deck/internal/debuglog"
)

// encodePathForDir encodes an absolute path into a directory-safe name.
// "/a/b/c" → "-a-b-c"
func encodePathForDir(absPath string) string {
	// 先頭 "/" を除去 → 残りの "/" を "-" に → 先頭に "-" を付加
	trimmed := strings.TrimPrefix(absPath, "/")
	encoded := strings.ReplaceAll(trimmed, "/", "-")
	return "-" + encoded
}

func (m *Manager) persist(sess *Session) {
	if m.store == nil {
		return
	}
	sess.mu.RLock()
	data, err := json.Marshal(sess, jsontext.WithIndent("  "))
	sess.mu.RUnlock()
	if err != nil {
		debuglog.Printf("[persist] session %s: JSON marshal failed: %v", sess.ID, err)
		return
	}
	if err := m.store.SaveBytes(sess.ID, data); err != nil {
		debuglog.Printf("[persist] session %s: store save failed: %v", sess.ID, err)
	}
}

// LoadExisting loads session metadata from the store.
// 直近30件だけ保持し、それより古いストアファイルは自動削除する。
func (m *Manager) LoadExisting() error {
	dataMap, err := m.store.LoadAll()
	if err != nil {
		return err
	}

	// 全件パースして sortTime で降順ソート
	type parsed struct {
		sess *Session
		id   string
	}
	all := make([]parsed, 0, len(dataMap))
	for id, data := range dataMap {
		var s Session
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		s.LogLines = make([]string, 0)

		// 前回起動時に実行中だったセッションはプロセスハンドルが失われている。
		// PID が生存していなければ完了扱いにする。
		if s.Status == StatusRunning || s.Status == StatusWaitingApproval || s.Status == StatusWaitingAnswer || s.Status == StatusIdle {
			if !isProcessAlive(s.PID) {
				s.Status = StatusCompleted
				now := time.Now()
				s.FinishedAt = &now
			}
		}

		all = append(all, parsed{sess: &s, id: id})
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].sess.sortTime().After(all[j].sess.sortTime())
	})

	m.mu.Lock()
	for i, p := range all {
		if i < m.config.MaxSessions {
			m.sessions[p.sess.ID] = p.sess
			// /clear で更新された旧 ID を oldSessionIDs に復元し、
			// DiscoverExternalSessions で再インポートされるのを防ぐ。
			if p.sess.PreviousClaudeSessionID != "" {
				if m.oldSessionIDs == nil {
					m.oldSessionIDs = make(map[string]bool)
				}
				m.oldSessionIDs[p.sess.PreviousClaudeSessionID] = true
			}
		} else {
			// 古いセッションはストアから削除
			_ = m.store.Delete(p.id)
		}
	}
	m.mu.Unlock()

	return nil
}

// pruneOldSessions removes the oldest store-backed sessions when exceeding maxSessions.
func (m *Manager) pruneOldSessions() {
	all := m.copySessionsList()

	if len(all) <= m.config.MaxSessions {
		return
	}

	// m.mu を解放してからソート（sortTime は s.mu を取るため ABBA 回避）
	sort.Slice(all, func(i, j int) bool {
		return all[i].sortTime().After(all[j].sortTime())
	})

	m.mu.Lock()
	for _, s := range all[m.config.MaxSessions:] {
		delete(m.sessions, s.ID)
		if m.store != nil {
			_ = m.store.Delete(s.ID)
		}
	}
	m.mu.Unlock()
}

// PersistAll saves all sessions to the store.
// claude-deck 終了時に呼び出し、TerminalTitle 等の実行時更新を永続化する。
func (m *Manager) PersistAll() {
	for _, s := range m.copySessionsList() {
		m.persist(s)
	}
}

// isProcessAlive checks if a process with the given PID is still running.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// signal 0 はシグナルを送らずにプロセスの存在だけを確認する
	return p.Signal(syscall.Signal(0)) == nil
}
