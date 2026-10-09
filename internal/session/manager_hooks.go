package session

import (
	"github.com/pomesaka/claude-deck/internal/store"
)

// The functions below are called by `claude-deck hook ...`, which the plugin
// runs from inside a Claude Code session. They write the store directly; the
// TUI picks the change up through WatchStore. No Manager is needed, so a hook
// does not touch tmux.

// RecordHookStatus applies a status reported by a hook. See applyHookStatus.
func RecordHookStatus(st *store.Store, sessionID DeckSessionID, status Status) error {
	_, err := st.Update(string(sessionID), func(r *store.Record) error {
		applyHookStatus(r, status)
		return nil
	})
	return err
}

// RecordSessionStart links the Claude Code session ID reported by SessionStart.
// See applySessionStart.
func RecordSessionStart(st *store.Store, sessionID DeckSessionID, claudeID ClaudeSessionID, source string) error {
	_, err := st.Update(string(sessionID), func(r *store.Record) error {
		applySessionStart(r, string(claudeID), source)
		return nil
	})
	return err
}
