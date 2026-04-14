package session

import (
	json "encoding/json/v2"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
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
	if err := m.store.SaveBytes(string(sess.ID), data); err != nil {
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

	// legacySession は旧 JSON 形式のフィールドを読み込むための一時構造体。
	// SessionChain 導入前は claude_session_id / previous_claude_session_id が別フィールドだった。
	type legacySession struct {
		ClaudeSessionID         string   `json:"claude_session_id,omitempty"`
		PreviousClaudeSessionID string   `json:"previous_claude_session_id,omitempty"`
		SessionChain            []string `json:"session_chain,omitempty"`
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
		s.rt.LogLines = make([]string, 0)

		// TODO: 旧形式 (claude_session_id / previous_claude_session_id) からの移行コード。
		// SessionChain 導入（v0.x）以前のストアデータ用。全セッションが一度でも起動されれば
		// 自動移行されるため、v1.0 リリース時に削除予定。
		if len(s.SessionChain) == 0 {
			var legacy legacySession
			if err := json.Unmarshal(data, &legacy); err == nil && legacy.ClaudeSessionID != "" {
				if legacy.PreviousClaudeSessionID != "" {
					s.SessionChain = []ClaudeSessionID{ClaudeSessionID(legacy.PreviousClaudeSessionID), ClaudeSessionID(legacy.ClaudeSessionID)}
				} else {
					s.SessionChain = []ClaudeSessionID{ClaudeSessionID(legacy.ClaudeSessionID)}
				}
			}
		}

		// 前回起動時に実行中だったセッションはプロセスハンドルが失われている。
		// PID が生存していなければ完了扱いにする。
		s.ReconcileStatusFromStore()

		// 作業ディレクトリが存在しないセッションをエラー状態にする
		if !isProcessAlive(s.PID) {
			workDir := s.WorkspacePath
			if workDir == "" {
				workDir = s.RepoPath
			}
			if workDir != "" {
				if _, statErr := os.Stat(workDir); os.IsNotExist(statErr) {
					s.Status = StatusError
					if s.FinishedAt == nil {
						now := time.Now()
						s.FinishedAt = &now
					}
					s.ErrorMessage = fmt.Sprintf("ディレクトリが見つかりません: %s", workDir)
				}
			}
		}

		all = append(all, parsed{sess: &s, id: id})
	}

	// SessionChain が長い順（より多くの履歴）→ 同じなら新しい順にソートする。
	// 重複排除でチェーンが長いほうを winner とするため、長い順を先頭にする。
	sort.SliceStable(all, func(i, j int) bool {
		li, lj := len(all[i].sess.SessionChain), len(all[j].sess.SessionChain)
		if li != lj {
			return li > lj
		}
		return all[i].sess.sortTime().After(all[j].sess.sortTime())
	})

	// 同一 Claude セッション ID を持つ重複エントリをストアから削除する。
	// DiscoverExternalSessions のレース（hook 未着火中に外部セッションとして二重登録）や
	// ReconcileTmux が Unmanaged → Completed に変換して保存する問題の後始末。
	// チェーンが長いものを winner とし、残りはストアから削除してスキップする。
	claimedClaudeIDs := make(map[ClaudeSessionID]bool)
	var deduped []parsed
	for _, p := range all {
		isDuplicate := false
		for _, csID := range p.sess.SessionChain {
			if csID != "" && claimedClaudeIDs[csID] {
				isDuplicate = true
				break
			}
		}
		if isDuplicate {
			debuglog.Printf("[LoadExisting] removing duplicate session %s from store", p.id)
			_ = m.store.Delete(p.id)
			continue
		}
		for _, csID := range p.sess.SessionChain {
			if csID != "" {
				claimedClaudeIDs[csID] = true
			}
		}
		deduped = append(deduped, p)
	}
	all = deduped

	m.mu.Lock()
	for i, p := range all {
		if i < m.config.MaxSessions {
			m.sessions[p.sess.ID] = p.sess
			// SessionChain に旧 ID が含まれるため oldSessionIDs は不要
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
			_ = m.store.Delete(string(s.ID))
		}
	}
	m.mu.Unlock()
}

// SyncNewFromStore loads any sessions from the store that are not yet in m.sessions,
// and updates SessionChain for sessions that are already in memory but have no chain.
//
// LoadExisting とは異なり、既存セッションの in-memory 状態（LogLines・process 等）を
// 上書きしない。preview プロセスなど、起動後に main プロセスが作成した新規セッションを
// 5秒 tick で追従するために使う。
//
// 呼び出し元は HydrateFromJSONL より前に呼ぶこと。そうすることで、新規追加セッションの
// JSONL データが同一 tick 内に読み込まれる。
func (m *Manager) SyncNewFromStore() {
	if m.store == nil {
		return
	}
	dataMap, err := m.store.LoadAll()
	if err != nil {
		return
	}

	for id, data := range dataMap {
		deckID := DeckSessionID(id)

		m.mu.RLock()
		existing, known := m.sessions[deckID]
		m.mu.RUnlock()

		var s Session
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}

		if known {
			// 既存セッション: SessionChain が in-memory で空だが disk にある場合だけ補完する。
			// main プロセスが ClaudeSessionID を persist した後に preview プロセスへ伝播する。
			existing.mu.Lock()
			if len(existing.SessionChain) == 0 && len(s.SessionChain) > 0 {
				existing.SessionChain = s.SessionChain
			}
			existing.mu.Unlock()
			continue
		}

		// 新規セッション: プロセスが存在しない実行中状態は Completed に補正する
		s.rt.LogLines = make([]string, 0)
		s.ReconcileStatusFromStore()
		m.mu.Lock()
		if _, exists := m.sessions[deckID]; !exists { // re-check under write lock
			m.sessions[deckID] = &s
		}
		m.mu.Unlock()
	}
}

// PersistAll saves all sessions to the store.
// claude-deck 終了時に呼び出し、TerminalTitle 等の実行時更新を永続化する。
// StatusUnmanaged（外部発見セッション）は次回起動時に JSONL から再発見されるため
// ストアへの保存を省略する。保存するとストアに重複エントリが蓄積する原因になる。
func (m *Manager) PersistAll() {
	for _, s := range m.copySessionsList() {
		s.mu.RLock()
		status := s.Status
		s.mu.RUnlock()
		if status == StatusUnmanaged {
			continue
		}
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
