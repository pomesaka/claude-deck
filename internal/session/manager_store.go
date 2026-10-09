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
// PRAGMA data_version は行を読まずに済むので、ステータス表示の遅れが目立たない短い間隔にする。
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

	var removed []DeckSessionID
	m.mu.Lock()
	for id, sess := range m.sessions {
		r, inStore := byID[id]
		// m.mu → s.mu の順（CLAUDE.md のロック順序）。
		sess.mu.Lock()
		switch {
		case inStore:
			sess.applyControlRecordLocked(r)
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
//  2. Row is unfinished, no window → the process ended while nothing recorded it; mark exited.
//  3. Window exists, no row → orphaned window; kill it.
//
// A CLI in another process may be launching a session at the same time, so
// each case reads the store and tmux in the order that cannot misjudge it.
func (m *Manager) ReconcileTmux() {
	// 1: the window is alive, so its row was written before; re-check the status in the transaction.
	if live, err := m.backend.LiveSessions(); err != nil {
		debuglog.Printf("[ReconcileTmux] listing windows failed: %v", err)
	} else {
		for id, pid := range live {
			if _, err := m.store.Update(string(id), func(r *store.Record) error {
				if before := r.Status; reviveForLiveWindow(r, pid) {
					debuglog.Printf("[ReconcileTmux] window alive but status=%s, reset to Idle session=%s", before, id)
				}
				return nil
			}); err != nil && !errors.Is(err, store.ErrNotFound) {
				debuglog.Printf("[ReconcileTmux] %s: %v", id, err)
			}
		}
	}

	// 2: same as the periodic check.
	m.markVanishedSessions()

	// 3: list windows before rows. A window another process creates is always
	// preceded by its row, so every window in this list that has a row has it in
	// the later store read.
	live, err := m.backend.LiveSessions()
	if err != nil {
		debuglog.Printf("[ReconcileTmux] listing windows failed: %v", err)
	} else if recs, err := m.store.List(); err != nil {
		debuglog.Printf("[ReconcileTmux] store list failed: %v", err)
	} else {
		known := make(map[DeckSessionID]bool, len(recs))
		for _, r := range recs {
			known[DeckSessionID(r.ID)] = true
		}
		var orphans []DeckSessionID
		for id := range live {
			if !known[id] {
				orphans = append(orphans, id)
			}
		}
		if err := m.backend.KillWindows(orphans); err != nil {
			debuglog.Printf("[ReconcileTmux] killing orphans: %v", err)
		}
	}
	m.Reload()
}

// markVanishedSessions marks unfinished sessions whose tmux window is gone as
// exited. It catches exits the pane's exit command did not record: the window
// was killed from tmux directly, or the claude-deck binary moved.
//
// Rows are read before windows: a row whose launch finishes in between then has
// its window in the list. A launch that starts in between is caught by
// markVanished re-reading the row.
func (m *Manager) markVanishedSessions() {
	recs, err := m.store.List()
	if err != nil {
		return
	}
	live, err := m.backend.LiveSessions()
	if err != nil {
		debuglog.Printf("[markVanishedSessions] listing windows failed: %v", err)
		return
	}
	now := time.Now()
	for _, r := range recs {
		if _, alive := live[DeckSessionID(r.ID)]; alive || !vanished(r, now) {
			continue
		}
		debuglog.Printf("[markVanishedSessions] window gone, marking exited session=%s", r.ID)
		if err := markVanished(m.store, m.usage, DeckSessionID(r.ID)); err != nil {
			debuglog.Printf("[markVanishedSessions] %s: %v", r.ID, err)
		}
	}
}

// pruneOldSessions deletes the oldest finished sessions beyond MaxSessions, with
// what they left on disk (see discardPruned).
// Unfinished sessions and sessions being closed are never pruned: their process
// may still be running, or a close is about to write the row. The choice is made
// inside one transaction so a concurrent resume or close is seen.
func (m *Manager) pruneOldSessions() {
	if m.config.MaxSessions <= 0 {
		return
	}
	var pruned, kept []store.Record
	if err := m.store.Tx(func(tx *store.Tx) error {
		pruned, kept = nil, nil
		recs, err := tx.List()
		if err != nil || len(recs) <= m.config.MaxSessions {
			return err
		}
		sort.Slice(recs, func(i, j int) bool {
			return recordSortTime(recs[i]).After(recordSortTime(recs[j]))
		})
		kept = append(kept, recs[:m.config.MaxSessions]...)
		now := time.Now()
		for _, r := range recs[m.config.MaxSessions:] {
			if status, ok := StatusFromID(r.Status); (ok && !status.IsTerminal()) || closingActive(r, now) {
				kept = append(kept, r)
				continue
			}
			if err := tx.Delete(r.ID); err != nil {
				return err
			}
			pruned = append(pruned, r)
		}
		return nil
	}); err != nil {
		debuglog.Printf("[pruneOldSessions] %v", err)
		return
	}
	// 行を消したプロセスだけが後片付けをする。jj とファイルの操作は時間がかかるので、
	// トランザクションの外で行う。
	for _, r := range pruned {
		m.ws().discardPruned(r, kept)
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
