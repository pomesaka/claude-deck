package session

import "testing"

// The old sessions/*.json files wrote the Status iota of the time. The numbers
// must map to the same meaning now that the order has changed.
func TestLegacyStatus(t *testing.T) {
	tests := []struct {
		n      int
		want   Status
		wantOK bool
	}{
		{0, StatusRunning, true},
		{1, StatusWaitingApproval, true},
		{2, StatusWaitingAnswer, true},
		{3, StatusCompleted, true},
		{4, StatusError, true},
		{5, StatusIdle, true},
		{6, StatusUnknown, false}, // external session: not imported
		{7, StatusSubagentRunning, true},
		{-1, StatusUnknown, false},
		{8, StatusUnknown, false},
	}
	for _, tt := range tests {
		got, ok := legacyStatus(tt.n)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("legacyStatus(%d) = %v, %v; want %v, %v", tt.n, got, ok, tt.want, tt.wantOK)
		}
	}
}

// Only deck sessions with a live process in a known directory get jj run for
// their bookmark.
func TestBookmarkTargets(t *testing.T) {
	snaps := []Snapshot{
		{ID: "live", Origin: OriginDeck, Status: StatusIdle, Isolation: DirectIsolation("/ws/a")},
		{ID: "running", Origin: OriginDeck, Status: StatusRunning, Isolation: DirectIsolation("/ws/b")},
		{ID: "done", Origin: OriginDeck, Status: StatusCompleted, Isolation: DirectIsolation("/ws/c")},
		{ID: "unknown", Origin: OriginDeck, Status: StatusUnknown, Isolation: DirectIsolation("/ws/d")},
		{ID: "external", Origin: OriginExternal, Status: StatusUnknown, Isolation: DirectIsolation("/elsewhere")},
		{ID: "no-dir", Origin: OriginDeck, Status: StatusIdle},
	}
	var got []DeckSessionID
	for _, s := range bookmarkTargets(snaps) {
		got = append(got, s.ID)
	}
	if want := []DeckSessionID{"live", "running"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("targets = %v, want %v", got, want)
	}
}
