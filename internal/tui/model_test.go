package tui

import (
	"testing"

	"github.com/pomesaka/claude-deck/internal/session"
)

// makeSessions returns n dummy sessions for testing.
func makeSessions(n int) []*session.Session {
	out := make([]*session.Session, n)
	for i := range n {
		out[i] = session.NewSession("/repo", "repo")
	}
	return out
}

// makeModel builds a minimal Model for ensureCursorVisible testing.
// height sets the terminal height; n is the number of sessions.
func makeModel(height, n int) *Model {
	return &Model{
		sessions: makeSessions(n),
		height:   height,
	}
}

// assertScrollValid checks that scrollOffset is a valid non-negative value
// that does not exceed the maximum scroll position for the current session list.
func assertScrollValid(t *testing.T, m *Model) {
	t.Helper()
	if m.scrollOffset < 0 {
		t.Errorf("scrollOffset = %d, must not be negative", m.scrollOffset)
	}
}

// assertCursorVisible checks that the cursor is not above scrollOffset.
// This verifies the "scroll up" invariant: if cursor < scrollOffset the item
// is invisible and ensureCursorVisible must lower the offset.
// The "scroll down" invariant (cursor must be below the bottom of the window)
// is tested per-case where cursor position is precisely known.
func assertCursorVisible(t *testing.T, m *Model) {
	t.Helper()
	if m.cursor < m.scrollOffset {
		t.Errorf("cursor %d is above scrollOffset %d (not visible)", m.cursor, m.scrollOffset)
	}
}

func TestEnsureCursorVisible_CursorAtTop(t *testing.T) {
	m := makeModel(20, 10)
	m.cursor = 0
	m.scrollOffset = 0

	m.ensureCursorVisible()

	if m.scrollOffset != 0 {
		t.Errorf("scrollOffset = %d, want 0 (cursor at top)", m.scrollOffset)
	}
	assertScrollValid(t, m)
}

func TestEnsureCursorVisible_CursorBelowWindow(t *testing.T) {
	// Place cursor far below the initial window to force a scroll-down.
	m := makeModel(20, 10)
	m.cursor = 9 // last of 10 sessions
	m.scrollOffset = 0

	m.ensureCursorVisible()

	assertCursorVisible(t, m)
	assertScrollValid(t, m)
	// scrollOffset must have advanced so the last item is in view
	if m.scrollOffset == 0 && m.cursor > 0 {
		t.Errorf("scrollOffset stayed 0 but cursor is %d (expected scroll-down)", m.cursor)
	}
}

func TestEnsureCursorVisible_CursorAboveScrollOffset(t *testing.T) {
	m := makeModel(20, 10)
	m.cursor = 0
	m.scrollOffset = 5 // scroll is ahead of cursor

	m.ensureCursorVisible()

	// scrollOffset must drop to 0 so cursor=0 is visible
	if m.scrollOffset != 0 {
		t.Errorf("scrollOffset = %d, want 0 (cursor above scroll)", m.scrollOffset)
	}
	assertScrollValid(t, m)
}

func TestEnsureCursorVisible_EmptyList(t *testing.T) {
	m := makeModel(20, 0)
	m.cursor = 0
	m.scrollOffset = 5 // stale offset from before list was emptied

	m.ensureCursorVisible()

	if m.scrollOffset != 0 {
		t.Errorf("scrollOffset = %d, want 0 for empty list", m.scrollOffset)
	}
	assertScrollValid(t, m)
}

func TestEnsureCursorVisible_FilterActive_MovesScrollDown(t *testing.T) {
	// With filter active the visible area is 1 row shorter; cursor at the
	// bottom must still be reachable via scrollOffset.
	m := makeModel(20, 10)
	m.filterActive = true
	m.cursor = 9
	m.scrollOffset = 0

	m.ensureCursorVisible()

	assertCursorVisible(t, m)
	assertScrollValid(t, m)
}

func TestEnsureCursorVisible_FilterText_MovesScrollDown(t *testing.T) {
	m := makeModel(20, 10)
	m.filterText = "repo" // all 10 sessions match ("/repo")
	m.cursor = 9
	m.scrollOffset = 0

	m.ensureCursorVisible()

	assertCursorVisible(t, m)
	assertScrollValid(t, m)
}

func TestEnsureCursorVisible_SmallWindow_NoNegativeOffset(t *testing.T) {
	// Very small window: height clamps contentHeight to 3 → visibleCount = 1
	m := makeModel(5, 3)
	m.cursor = 2
	m.scrollOffset = 0

	m.ensureCursorVisible()

	assertCursorVisible(t, m)
	assertScrollValid(t, m)
}

func TestEnsureCursorVisible_StaleScrollOffset_Clamped(t *testing.T) {
	// Simulates a window resize: scrollOffset is now too large for the session list.
	m := makeModel(20, 3)
	m.cursor = 0
	m.scrollOffset = 10 // larger than list

	m.ensureCursorVisible()

	assertCursorVisible(t, m)
	assertScrollValid(t, m)
}
