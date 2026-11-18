package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/pomesaka/claude-deck/internal/session"
)

func TestRightPaneTargetForDisplay(t *testing.T) {
	tests := []struct {
		name    string
		display session.DisplayChannel
		want    rightPaneTarget
	}{
		{name: "running_session_uses_tmux", display: session.DisplayTmux, want: rightPaneTmux},
		{name: "archived_session_uses_preview", display: session.DisplayJSONL, want: rightPanePreview},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rightPaneTargetForDisplay(tt.display); got != tt.want {
				t.Fatalf("rightPaneTargetForDisplay(%v) = %v, want %v", tt.display, got, tt.want)
			}
		})
	}
}

func TestBuildRightPaneSwitchFromSnap_RunningSessionUsesTmux(t *testing.T) {
	m := &Model{}
	got := m.buildRightPaneSwitchFromSnap(session.Snapshot{
		ID:      session.DeckSessionID("running"),
		Display: session.DisplayTmux,
	}, false)

	if got.SessionID != "running" {
		t.Fatalf("SessionID = %q, want running", got.SessionID)
	}
	if got.Target != rightPaneTmux {
		t.Fatalf("Target = %v, want rightPaneTmux", got.Target)
	}
	if got.Spec.Display != "" {
		t.Fatalf("Spec.Display = %q, want empty for tmux target", got.Spec.Display)
	}
	if got.Generation == 0 {
		t.Fatal("Generation = 0, want non-zero")
	}
}

func TestShouldApplyRightPaneSwitchDropsStaleCursorMove(t *testing.T) {
	m := &Model{selectedID: session.DeckSessionID("current")}
	sw := rightPaneSwitch{
		SessionID:  session.DeckSessionID("old"),
		FocusRight: false,
		Target:     rightPaneTmux,
	}

	if m.shouldApplyRightPaneSwitch(sw) {
		t.Fatal("shouldApplyRightPaneSwitch = true, want false for stale cursor move")
	}
}

func TestShouldApplyRightPaneSwitchKeepsExplicitFocus(t *testing.T) {
	m := &Model{selectedID: session.DeckSessionID("current")}
	sw := rightPaneSwitch{
		SessionID:  session.DeckSessionID("old"),
		FocusRight: true,
		Target:     rightPaneTmux,
	}

	if !m.shouldApplyRightPaneSwitch(sw) {
		t.Fatal("shouldApplyRightPaneSwitch = false, want true for explicit focus")
	}
}

// The status line says when close kept the workspace, and where.
func TestUpdate_SessionKilledStatus(t *testing.T) {
	tests := []struct {
		name    string
		msg     sessionKilledMsg
		want    []string
		notWant []string
	}{
		{name: "removed", msg: sessionKilledMsg{}, want: []string{"終了しました"}, notWant: []string{"残しました"}},
		{
			name:    "kept",
			msg:     sessionKilledMsg{result: session.CloseResult{WorkspaceKept: "/data/workspace/x/ws"}},
			want:    []string{"残しました"},
			notWant: []string{"削除済"},
		},
		{name: "error", msg: sessionKilledMsg{err: errors.New("boom")}, want: []string{"終了エラー", "boom"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next, _ := Model{killing: true}.Update(tt.msg)
			m, ok := next.(Model)
			if !ok {
				t.Fatalf("Update returned %T", next)
			}
			if m.killing {
				t.Error("killing still set")
			}
			for _, w := range tt.want {
				if !strings.Contains(m.statusMsg, w) {
					t.Errorf("statusMsg = %q, want it to contain %q", m.statusMsg, w)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(m.statusMsg, w) {
					t.Errorf("statusMsg = %q, want it not to contain %q", m.statusMsg, w)
				}
			}
		})
	}
}
