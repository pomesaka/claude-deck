package session

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/store"
)

// storeWatchInterval is how often the TUI checks the store for changes made by
// other processes (CLI, hook commands, the pane's exit command).
// PRAGMA data_version は 1 回数十マイクロ秒なので、ステータス表示の遅れが目立たない間隔にする。
const storeWatchInterval = 200 * time.Millisecond

// encodePathForDir encodes an absolute path into a directory-safe name.
// "/a/b/c" → "-a-b-c"
func encodePathForDir(absPath string) string {
	// 先頭 "/" を除去 → 残りの "/" を "-" に → 先頭に "-" を付加
	trimmed := strings.TrimPrefix(absPath, "/")
	encoded := strings.ReplaceAll(trimmed, "/", "-")
	return "-" + encoded
}

// OpenStore opens the session store under dataDir, importing the JSON files of
// the previous store format on first use.
func OpenStore(dataDir string) (*store.Store, error) {
	st, err := store.Open(dataDir)
	if err != nil {
		return nil, err
	}
	if err := migrateLegacyStore(st, dataDir); err != nil {
		// 移行に失敗しても新しい store は使える。古いファイルは残るので次回また試す。
		debuglog.Printf("[OpenStore] legacy migration failed: %v", err)
	}
	return st, nil
}

// legacySessionsDir is where the JSON store (before ADR-011) kept one file per session.
func legacySessionsDir(dataDir string) string {
	return filepath.Join(dataDir, "sessions")
}

// migrateLegacyStore imports dataDir/sessions/*.json into an empty store, then
// renames the directory so the import runs only once.
func migrateLegacyStore(st *store.Store, dataDir string) error {
	dir := legacySessionsDir(dataDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	var recs []store.Record
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var s Session
		if err := json.Unmarshal(data, &s); err != nil || s.ID == "" {
			continue
		}
		// 外部セッションは JSONL から毎回発見し直すもので、store には持たない。
		if s.Status == StatusUnmanaged {
			continue
		}
		recs = append(recs, s.recordLocked())
	}

	// 別プロセスが同時に移行しても二重に入らないよう、空であることの確認と挿入を 1 つのトランザクションにする。
	if err := st.Tx(func(tx *store.Tx) error {
		existing, err := tx.List()
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			return nil
		}
		for _, r := range recs {
			if err := tx.Put(r); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	migrated := dir + ".migrated-" + time.Now().Format("20060102-150405")
	if err := os.Rename(dir, migrated); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("renaming %s: %w", dir, err)
	}
	debuglog.Printf("[migrateLegacyStore] imported %d sessions, moved %s to %s", len(recs), dir, migrated)
	return nil
}

// Reload replaces the in-memory projection of deck sessions with the store's
// current contents. External sessions (memory only) are kept, except those now
// owned by a deck session.
//
// Store-written fields (status, chain, workspace, …) always follow the store.
// Fields the TUI projects from JSONL and jj are taken from the store only when a
// session first appears; afterwards the in-memory values are the newer ones.
func (m *Manager) Reload() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	recs, err := m.store.List()
	if err != nil {
		debuglog.Printf("[Reload] store list failed: %v", err)
		return
	}
	byID := make(map[DeckSessionID]store.Record, len(recs))
	ownedClaudeIDs := make(map[ClaudeSessionID]bool)
	for _, r := range recs {
		byID[DeckSessionID(r.ID)] = r
		for _, id := range r.SessionChain {
			ownedClaudeIDs[ClaudeSessionID(id)] = true
		}
	}

	var chainChanged, removed []DeckSessionID
	m.mu.Lock()
	for id, sess := range m.sessions {
		r, inStore := byID[id]
		// m.mu → s.mu の順（CLAUDE.md のロック順序）。
		sess.mu.Lock()
		switch {
		case inStore:
			if sess.applyControlRecordLocked(r) {
				chainChanged = append(chainChanged, id)
			}
		case sess.Status == StatusUnmanaged:
			// 外部セッションは、同じ Claude セッションを deck セッションが持つようになったら消す
			// （hook で ID が届く前に discovery が外部セッションとして取り込んでいた場合）。
			for _, cid := range sess.SessionChain {
				if ownedClaudeIDs[cid] {
					removed = append(removed, id)
					break
				}
			}
		default:
			// 別のプロセスが store から削除した
			removed = append(removed, id)
		}
		sess.mu.Unlock()
	}
	for _, id := range removed {
		delete(m.sessions, id)
	}
	for id, r := range byID {
		if _, ok := m.sessions[id]; !ok {
			m.sessions[id] = newSessionFromRecord(r)
		}
	}
	m.mu.Unlock()

	for _, id := range removed {
		m.stopActiveStream(id)
	}
	// /clear で現在の Claude セッション ID が変わったら、ログを新しいセッションのものに切り替える。
	for _, id := range chainChanged {
		sess := m.GetSession(id)
		if sess == nil {
			continue
		}
		sess.rt.mu.Lock()
		sess.rt.JSONLLogEntries = nil
		sess.rt.mu.Unlock()
		if m.stream.isCurrent(id) {
			m.stopActiveStream(id)
			m.StreamSession(id)
		}
	}
	m.notifyChange()
}

// WatchStore reloads whenever another process changes the store. Blocks until
// ctx is cancelled, so run it in a goroutine.
func (m *Manager) WatchStore(ctx context.Context) {
	last, err := m.store.DataVersion()
	if err != nil {
		debuglog.Printf("[WatchStore] data_version: %v", err)
	}
	ticker := time.NewTicker(storeWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			v, err := m.store.DataVersion()
			if err != nil {
				debuglog.Printf("[WatchStore] data_version: %v", err)
				continue
			}
			if v != last {
				last = v
				m.Reload()
			}
		}
	}
}

// LoadExisting prepares the store at TUI startup and loads it into memory:
// removes duplicates, prunes old sessions, and marks finished sessions whose
// directory is gone as errors.
func (m *Manager) LoadExisting() error {
	recs, err := m.store.List()
	if err != nil {
		return err
	}

	// SessionChain が長い順（より多くの履歴）→ 同じなら新しい順にソートする。
	// 重複排除でチェーンが長いほうを winner とするため、長い順を先頭にする。
	sort.SliceStable(recs, func(i, j int) bool {
		li, lj := len(recs[i].SessionChain), len(recs[j].SessionChain)
		if li != lj {
			return li > lj
		}
		return recordSortTime(recs[i]).After(recordSortTime(recs[j]))
	})

	// 同一 Claude セッション ID を持つ重複エントリを削除する（JSON の store 時代に蓄積したもの）。
	claimed := make(map[string]bool)
	for _, r := range recs {
		dup := false
		for _, id := range r.SessionChain {
			if claimed[id] {
				dup = true
				break
			}
		}
		if dup {
			debuglog.Printf("[LoadExisting] removing duplicate session %s from store", r.ID)
			_ = m.store.Delete(r.ID)
			continue
		}
		for _, id := range r.SessionChain {
			claimed[id] = true
		}

		// 作業ディレクトリが存在しない終了済みセッションをエラー状態にする。
		// 実行中のものは ReconcileTmux がウィンドウの有無で判定する。
		if status, ok := StatusFromID(r.Status); ok && status == StatusCompleted {
			workDir := r.WorkspacePath
			if workDir == "" {
				workDir = r.RepoPath
			}
			if workDir != "" {
				if _, statErr := os.Stat(workDir); os.IsNotExist(statErr) {
					msg := fmt.Sprintf("ディレクトリが見つかりません: %s", workDir)
					if _, err := m.store.Update(r.ID, func(r *store.Record) error {
						setError(r, msg, time.Now())
						return nil
					}); err != nil {
						debuglog.Printf("[LoadExisting] marking %s as error: %v", r.ID, err)
					}
				}
			}
		}
	}

	m.pruneOldSessions()
	m.Reload()
	return nil
}

// ReconcileTmux aligns the store with the live tmux windows at TUI startup.
//
//  1. Window exists, row is finished → the row went stale (e.g. written by an
//     older claude-deck); set it back to Idle.
//  2. Window exists, no row → orphaned window; kill it.
//  3. Row is unfinished, no window → the process ended while nothing recorded it; mark exited.
func (m *Manager) ReconcileTmux() {
	live, err := m.backend.LiveSessions()
	if err != nil {
		debuglog.Printf("[ReconcileTmux] listing windows failed: %v", err)
		return
	}
	recs, err := m.store.List()
	if err != nil {
		debuglog.Printf("[ReconcileTmux] store list failed: %v", err)
		return
	}
	known := make(map[DeckSessionID]bool, len(recs))
	for _, r := range recs {
		id := DeckSessionID(r.ID)
		known[id] = true
		status, ok := StatusFromID(r.Status)
		if !ok || status == StatusUnmanaged {
			continue
		}
		pid, alive := live[id]
		switch {
		case alive && status.IsTerminal():
			debuglog.Printf("[ReconcileTmux] window alive but status=%s, resetting to Idle session=%s", status, id)
			if _, err := m.store.Update(r.ID, func(r *store.Record) error {
				r.Status = StatusIdle.ID()
				r.FinishedAt = nil
				r.ErrorMessage = ""
				r.PID = pid
				return nil
			}); err != nil {
				debuglog.Printf("[ReconcileTmux] %s: %v", id, err)
			}
		case !alive && !status.IsTerminal():
			debuglog.Printf("[ReconcileTmux] window gone, marking exited session=%s", id)
			if err := MarkExited(m.store, m.usage, id); err != nil {
				debuglog.Printf("[ReconcileTmux] %s: %v", id, err)
			}
		}
	}
	if err := m.backend.KillOrphans(known); err != nil {
		debuglog.Printf("[ReconcileTmux] killing orphans: %v", err)
	}
	m.Reload()
}

// markVanishedSessions marks unfinished sessions whose tmux window is gone as
// exited. It catches exits the pane's exit command did not record: the window
// was killed from tmux directly, or the claude-deck binary moved.
//
// Sessions with PID 0 are skipped: their launch is still in progress in some
// process and the window may not exist yet.
func (m *Manager) markVanishedSessions() {
	live, err := m.backend.LiveSessions()
	if err != nil {
		debuglog.Printf("[markVanishedSessions] listing windows failed: %v", err)
		return
	}
	recs, err := m.store.List()
	if err != nil {
		return
	}
	for _, r := range recs {
		status, ok := StatusFromID(r.Status)
		if !ok || status.IsTerminal() || status == StatusUnmanaged || r.PID == 0 || r.ClosingAt != nil {
			continue
		}
		if _, alive := live[DeckSessionID(r.ID)]; alive {
			continue
		}
		debuglog.Printf("[markVanishedSessions] window gone, marking exited session=%s", r.ID)
		if err := MarkExited(m.store, m.usage, DeckSessionID(r.ID)); err != nil {
			debuglog.Printf("[markVanishedSessions] %s: %v", r.ID, err)
		}
	}
}

// pruneOldSessions deletes the oldest finished sessions beyond MaxSessions.
// Unfinished sessions are never pruned: their process may still be running.
func (m *Manager) pruneOldSessions() {
	if m.config.MaxSessions <= 0 {
		return
	}
	recs, err := m.store.List()
	if err != nil || len(recs) <= m.config.MaxSessions {
		return
	}
	sort.Slice(recs, func(i, j int) bool {
		return recordSortTime(recs[i]).After(recordSortTime(recs[j]))
	})
	for _, r := range recs[m.config.MaxSessions:] {
		if status, ok := StatusFromID(r.Status); ok && !status.IsTerminal() {
			continue
		}
		_ = m.store.Delete(r.ID)
	}
}

// recordSortTime mirrors Session.sortTime for store rows.
func recordSortTime(r store.Record) time.Time {
	if !r.LastActivity.IsZero() {
		return r.LastActivity
	}
	if r.FinishedAt != nil {
		return *r.FinishedAt
	}
	return r.StartedAt
}

// PersistAll saves the TUI-projected fields (tokens, prompt, timestamps, bookmark)
// of every deck session, so the next start shows them before JSONL is read again.
func (m *Manager) PersistAll() {
	for _, s := range m.copySessionsList() {
		s.mu.RLock()
		status := s.Status
		src := s.recordLocked()
		s.mu.RUnlock()
		if status == StatusUnmanaged {
			continue
		}
		if _, err := m.store.Update(src.ID, func(r *store.Record) error {
			copyProjection(r, src)
			return nil
		}); err != nil && !errors.Is(err, store.ErrNotFound) {
			debuglog.Printf("[PersistAll] %s: %v", src.ID, err)
		}
	}
}
