package session

import (
	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/hooks"
)

// hookProcessor owns the SessionEnd→SessionStart pairing state machine.
//
// /clear や compact では以下の順序でイベントが発火する:
//  1. SessionEnd  {session_id: OLD, reason: "clear", claude_deck_session_id: DECK}
//  2. SessionStart {session_id: NEW, source: "clear", claude_deck_session_id: DECK}
//
// hookProcessor は SessionEnd を pendingEnd に蓄積し、
// 後続の SessionStart でそれを消費して clear ペアを確定する。
//
// シングルゴルーチン前提: Manager の event watcher goroutine のみが呼び出す。
// mu 不要（pendingEnd は hookProcessor のみが読み書きする）。
type hookProcessor struct {
	pendingEnd map[string]*hooks.Event // keyed by ClaudeDeckSessionID
}

func newHookProcessor() *hookProcessor {
	return &hookProcessor{
		pendingEnd: make(map[string]*hooks.Event),
	}
}

// storePending records a SessionEnd event awaiting its paired SessionStart.
// 同一 deckSessionID で複数回呼ばれた場合は最新のイベントで上書きする。
func (h *hookProcessor) storePending(deckSessionID string, ev *hooks.Event) {
	h.pendingEnd[deckSessionID] = ev
	debuglog.Printf("[hook-proc] storePending: deck=%s claude=%s reason=%s", deckSessionID, ev.SessionID, ev.Reason)
}

// consumePending takes and removes the pending SessionEnd for deckSessionID.
// Returns nil if no pending event exists.
func (h *hookProcessor) consumePending(deckSessionID string) *hooks.Event {
	ev := h.pendingEnd[deckSessionID]
	delete(h.pendingEnd, deckSessionID)
	if ev != nil {
		debuglog.Printf("[hook-proc] consumePending: deck=%s claude=%s", deckSessionID, ev.SessionID)
	}
	return ev
}

// hasPending reports whether a pending SessionEnd exists for deckSessionID.
// テスト専用。
func (h *hookProcessor) hasPending(deckSessionID string) bool {
	_, ok := h.pendingEnd[deckSessionID]
	return ok
}

// pendingCount returns the number of stored pending events (for tests).
func (h *hookProcessor) pendingCount() int {
	return len(h.pendingEnd)
}
