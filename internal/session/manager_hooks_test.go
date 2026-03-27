package session

import (
	"context"
	"testing"

	"github.com/pomesaka/claude-deck/internal/hooks"
	"github.com/pomesaka/claude-deck/internal/pty"
	"github.com/pomesaka/claude-deck/internal/usage"
)

// newTestManager creates a Manager with no store or external dependencies.
// Assumption: the methods under test (handleHookEvent and findSessionByClaudeID)
// do not access m.store, m.usage, or m.config. If that changes, this helper
// must be updated to avoid silent nil panics.
func newTestManager() *Manager {
	return &Manager{
		sessions:  make(map[string]*Session),
		processes: make(map[string]*pty.Process),
		ctx:       context.Background(),
	}
}

// addTestSession inserts a session into the manager with a preset ID and Claude session ID.
func addTestSession(m *Manager, deckID, claudeSessionID string) *Session {
	sess := NewSession("/repo", "repo")
	sess.ID = deckID
	if claudeSessionID != "" {
		sess.SessionChain = []string{claudeSessionID}
	}
	m.sessions[deckID] = sess
	return sess
}

// TestHandleHookEvent_SessionStart_Startup tests that a startup SessionStart
// sets ClaudeSessionID on a session that has none yet.
func TestHandleHookEvent_SessionStart_Startup(t *testing.T) {
	m := newTestManager()
	sess := addTestSession(m, "deck-1", "")

	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionStart,
		SessionID:           "claude-abc",
		Source:              hooks.SourceStartup,
		ClaudeDeckSessionID: "deck-1",
	})

	sess.mu.RLock()
	got := sess.CurrentClaudeID()
	sess.mu.RUnlock()

	if got != "claude-abc" {
		t.Errorf("CurrentClaudeID = %q, want %q", got, "claude-abc")
	}
}

// TestHandleHookEvent_SessionStart_Startup_AlreadySet tests that a startup
// SessionStart is skipped if the session already has a ClaudeSessionID.
func TestHandleHookEvent_SessionStart_Startup_AlreadySet(t *testing.T) {
	m := newTestManager()
	sess := addTestSession(m, "deck-1", "claude-existing")

	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionStart,
		SessionID:           "claude-new",
		Source:              hooks.SourceStartup,
		ClaudeDeckSessionID: "deck-1",
	})

	sess.mu.RLock()
	got := sess.CurrentClaudeID()
	sess.mu.RUnlock()

	if got != "claude-existing" {
		t.Errorf("CurrentClaudeID should not be overwritten, got %q", got)
	}
}

// TestHandleHookEvent_SessionStart_Startup_NoClaudeDeckID tests that
// a startup SessionStart without ClaudeDeckSessionID is ignored.
func TestHandleHookEvent_SessionStart_Startup_NoClaudeDeckID(t *testing.T) {
	m := newTestManager()
	sess := addTestSession(m, "deck-1", "")

	m.handleHookEvent(hooks.Event{
		HookEventName: hooks.EventSessionStart,
		SessionID:     "claude-abc",
		Source:        hooks.SourceStartup,
		// ClaudeDeckSessionID intentionally empty
	})

	sess.mu.RLock()
	got := sess.CurrentClaudeID()
	sess.mu.RUnlock()

	if got != "" {
		t.Errorf("CurrentClaudeID should remain empty, got %q", got)
	}
}

// TestHandleHookEvent_Clear_PairsEndAndStart tests the full /clear flow:
// SessionEnd (old ID) then SessionStart (new ID) with source=clear should
// append the new ID to SessionChain and keep the old ID as history.
func TestHandleHookEvent_Clear_PairsEndAndStart(t *testing.T) {
	m := newTestManager()
	sess := addTestSession(m, "deck-1", "claude-old")

	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionEnd,
		SessionID:           "claude-old",
		Reason:              "clear",
		ClaudeDeckSessionID: "deck-1",
	})
	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionStart,
		SessionID:           "claude-new",
		Source:              hooks.SourceClear,
		ClaudeDeckSessionID: "deck-1",
	})

	sess.mu.RLock()
	newCSID := sess.CurrentClaudeID()
	priorIDs := sess.PriorClaudeIDs()
	sess.mu.RUnlock()

	if newCSID != "claude-new" {
		t.Errorf("CurrentClaudeID = %q, want %q", newCSID, "claude-new")
	}
	if len(priorIDs) != 1 || priorIDs[0] != "claude-old" {
		t.Errorf("PriorClaudeIDs = %v, want [claude-old]", priorIDs)
	}
}

// TestHandleHookEvent_Clear_SameID tests that a /clear that results in the
// same session ID (no-op) does not extend the chain.
func TestHandleHookEvent_Clear_SameID(t *testing.T) {
	m := newTestManager()
	sess := addTestSession(m, "deck-1", "claude-same")

	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionEnd,
		SessionID:           "claude-same",
		Reason:              "clear",
		ClaudeDeckSessionID: "deck-1",
	})
	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionStart,
		SessionID:           "claude-same",
		Source:              hooks.SourceClear,
		ClaudeDeckSessionID: "deck-1",
	})

	sess.mu.RLock()
	priorIDs := sess.PriorClaudeIDs()
	sess.mu.RUnlock()

	// SessionChain should not grow (same ID = no-op via appendToChainLocked)
	if len(priorIDs) != 0 {
		t.Errorf("PriorClaudeIDs should be empty for same ID, got %v", priorIDs)
	}
}

// TestHandleHookEvent_Clear_NoPendingEnd tests that a SessionStart with
// source=clear but no matching pending SessionEnd is silently ignored.
func TestHandleHookEvent_Clear_NoPendingEnd(t *testing.T) {
	m := newTestManager()
	sess := addTestSession(m, "deck-1", "claude-old")

	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionStart,
		SessionID:           "claude-new",
		Source:              hooks.SourceClear,
		ClaudeDeckSessionID: "deck-1",
	})

	sess.mu.RLock()
	got := sess.CurrentClaudeID()
	sess.mu.RUnlock()

	if got != "claude-old" {
		t.Errorf("CurrentClaudeID should not change without pending SessionEnd, got %q", got)
	}
}

// TestHandleHookEvent_SessionEnd_NoClaudeDeckID tests that a SessionEnd
// without ClaudeDeckSessionID is ignored (cannot pair without it).
func TestHandleHookEvent_SessionEnd_NoClaudeDeckID(t *testing.T) {
	m := newTestManager()

	m.handleHookEvent(hooks.Event{
		HookEventName: hooks.EventSessionEnd,
		SessionID:     "claude-old",
		Reason:        "clear",
		// ClaudeDeckSessionID intentionally empty
	})

	m.mu.RLock()
	pending := m.pendingEndEvents
	m.mu.RUnlock()

	// Nothing should be stored in pendingEndEvents
	if len(pending) != 0 {
		t.Errorf("pendingEndEvents should be empty, got %v", pending)
	}
}

// TestHandleHookEvent_Clear_OldIDTracked tests that after a /clear,
// the old Claude session ID is included in knownClaudeSessionIDs to prevent re-import.
// With SessionChain, the old ID lives in the chain itself — no separate map needed.
func TestHandleHookEvent_Clear_OldIDTracked(t *testing.T) {
	m := newTestManager()
	addTestSession(m, "deck-1", "claude-old")

	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionEnd,
		SessionID:           "claude-old",
		Reason:              "clear",
		ClaudeDeckSessionID: "deck-1",
	})
	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionStart,
		SessionID:           "claude-new",
		Source:              hooks.SourceClear,
		ClaudeDeckSessionID: "deck-1",
	})

	known := m.knownClaudeSessionIDs()

	if !known["claude-old"] {
		t.Error("old Claude session ID should be in knownClaudeSessionIDs after /clear")
	}
	if !known["claude-new"] {
		t.Error("new Claude session ID should be in knownClaudeSessionIDs after /clear")
	}
}

// TestHandleHookEvent_Clear_JSONLEntriesReset tests that /clear resets
// JSONLLogEntries so the detail pane shows only the new session's log.
func TestHandleHookEvent_Clear_JSONLEntriesReset(t *testing.T) {
	m := newTestManager()
	sess := addTestSession(m, "deck-1", "claude-old")

	sess.mu.Lock()
	sess.JSONLLogEntries = []usage.LogEntry{{Kind: usage.LogEntryUser, Text: "old message"}}
	sess.mu.Unlock()

	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionEnd,
		SessionID:           "claude-old",
		Reason:              "clear",
		ClaudeDeckSessionID: "deck-1",
	})
	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionStart,
		SessionID:           "claude-new",
		Source:              hooks.SourceClear,
		ClaudeDeckSessionID: "deck-1",
	})

	sess.mu.RLock()
	entries := sess.JSONLLogEntries
	sess.mu.RUnlock()

	if entries != nil {
		t.Errorf("JSONLLogEntries should be nil after /clear, got len=%d", len(entries))
	}
}

// TestHandleHookEvent_Compact_PairsEndAndStart tests the compact flow
// (identical to /clear pairing but with source=compact).
func TestHandleHookEvent_Compact_PairsEndAndStart(t *testing.T) {
	m := newTestManager()
	sess := addTestSession(m, "deck-1", "claude-old")

	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionEnd,
		SessionID:           "claude-old",
		Reason:              "compact",
		ClaudeDeckSessionID: "deck-1",
	})
	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionStart,
		SessionID:           "claude-new",
		Source:              hooks.SourceCompact,
		ClaudeDeckSessionID: "deck-1",
	})

	sess.mu.RLock()
	newCSID := sess.CurrentClaudeID()
	priorIDs := sess.PriorClaudeIDs()
	sess.mu.RUnlock()

	if newCSID != "claude-new" {
		t.Errorf("CurrentClaudeID = %q, want %q", newCSID, "claude-new")
	}
	if len(priorIDs) != 1 || priorIDs[0] != "claude-old" {
		t.Errorf("PriorClaudeIDs = %v, want [claude-old]", priorIDs)
	}
}

// TestHandleHookEvent_MultiClear_ChainGrows tests that multiple /clear operations
// accumulate all historical IDs in SessionChain.
func TestHandleHookEvent_MultiClear_ChainGrows(t *testing.T) {
	m := newTestManager()
	sess := addTestSession(m, "deck-1", "claude-1")

	// First /clear: 1 → 2
	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionEnd,
		SessionID:           "claude-1",
		Reason:              "clear",
		ClaudeDeckSessionID: "deck-1",
	})
	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionStart,
		SessionID:           "claude-2",
		Source:              hooks.SourceClear,
		ClaudeDeckSessionID: "deck-1",
	})

	// Second /clear: 2 → 3
	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionEnd,
		SessionID:           "claude-2",
		Reason:              "clear",
		ClaudeDeckSessionID: "deck-1",
	})
	m.handleHookEvent(hooks.Event{
		HookEventName:       hooks.EventSessionStart,
		SessionID:           "claude-3",
		Source:              hooks.SourceClear,
		ClaudeDeckSessionID: "deck-1",
	})

	sess.mu.RLock()
	chain := make([]string, len(sess.SessionChain))
	copy(chain, sess.SessionChain)
	sess.mu.RUnlock()

	want := []string{"claude-1", "claude-2", "claude-3"}
	if len(chain) != len(want) {
		t.Fatalf("SessionChain = %v, want %v", chain, want)
	}
	for i, id := range want {
		if chain[i] != id {
			t.Errorf("SessionChain[%d] = %q, want %q", i, chain[i], id)
		}
	}

	// All IDs should be known
	known := m.knownClaudeSessionIDs()
	for _, id := range want {
		if !known[id] {
			t.Errorf("knownClaudeSessionIDs missing %q", id)
		}
	}
}
