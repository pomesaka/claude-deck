package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/pomesaka/claude-deck/internal/config"
	"github.com/pomesaka/claude-deck/internal/session"
)

// clickedSession runs the view's mouse handler for a click on screen row y and
// returns the session it reports, or "".
func clickedSession(v tea.View, button tea.MouseButton, y int) session.DeckSessionID {
	cmd := v.OnMouse(tea.MouseClickMsg{X: 5, Y: y, Button: button})
	if cmd == nil {
		return ""
	}
	msg, ok := cmd().(sessionClickedMsg)
	if !ok {
		return ""
	}
	return msg.id
}

// The view's mouse handler must report the session that is drawn on the clicked
// row. The rows are checked against the rendered screen by each session's name.
func TestView_ClickReportsTheSessionOnThatRow(t *testing.T) {
	tests := []struct {
		name         string
		height       int
		sessions     int
		scrollOffset int
		filterText   string
		// wantRows maps a session index to the screen row of its first line.
		wantRows map[int]int
	}{
		{"few sessions sit at the bottom", 20, 3, 0, "", map[int]int{0: 12, 1: 14, 2: 16}},
		{"more below", 12, 10, 0, "", map[int]int{0: 3, 1: 5, 2: 7}},
		{"scrolled to the middle", 12, 10, 3, "", map[int]int{3: 3, 4: 5, 5: 7}},
		{"scrolled to the end", 12, 10, 7, "", map[int]int{7: 4, 8: 6, 9: 8}},
		{"filter bar under the list", 20, 3, 0, "x", map[int]int{0: 11, 1: 13, 2: 15}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snaps := make([]session.Snapshot, tt.sessions)
			for i := range snaps {
				snaps[i] = session.Snapshot{
					ID:   session.DeckSessionID(fmt.Sprintf("id-%02d", i)),
					Name: fmt.Sprintf("sess-%02d", i), RepoName: "repo", Status: session.StatusIdle,
				}
			}
			m := Model{
				config: config.Default(), width: 80, height: tt.height,
				viewSnaps: snaps, scrollOffset: tt.scrollOffset, filterText: tt.filterText,
			}
			v := m.View()
			screen := strings.Split(v.Content, "\n")

			gotRows := map[int]int{}
			for y, line := range screen {
				for i := range snaps {
					if strings.Contains(line, snaps[i].Name) {
						gotRows[i] = y
					}
				}
			}
			if fmt.Sprint(gotRows) != fmt.Sprint(tt.wantRows) {
				t.Fatalf("rendered rows = %v, want %v\n%s", gotRows, tt.wantRows, v.Content)
			}

			// Every screen row reports the session drawn on it, or none.
			wantAt := map[int]session.DeckSessionID{}
			for i, y := range tt.wantRows {
				wantAt[y], wantAt[y+1] = snaps[i].ID, snaps[i].ID
			}
			for y := -1; y <= len(screen); y++ {
				if got := clickedSession(v, tea.MouseLeft, y); got != wantAt[y] {
					t.Errorf("click on row %d reports %q, want %q", y, got, wantAt[y])
				}
			}
			if got := clickedSession(v, tea.MouseRight, tt.wantRows[tt.scrollOffset]); got != "" {
				t.Errorf("right click reports %q, want none", got)
			}
		})
	}
}

// Outside the dashboard the view passes no rows, so no click reports a session.
func TestSessionClickHandler_NoRows(t *testing.T) {
	v := tea.View{OnMouse: sessionClickHandler(nil, 1)}
	for y := range 5 {
		if got := clickedSession(v, tea.MouseLeft, y); got != "" {
			t.Errorf("click on row %d reports %q, want none", y, got)
		}
	}
}
