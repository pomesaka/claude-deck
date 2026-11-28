package tui

import (
	"errors"
	"strings"
	"testing"
)

// A reload quits the TUI with the binary to start from and the session the cursor was on.
func TestUpdate_Reload(t *testing.T) {
	d := newDashboard(t, 3, 20)
	d.key("k")
	want := d.m.selectedID
	d.send(ReloadMsg{Binary: "/bin/new", Refuse: func(err error) { t.Errorf("refused: %v", err) }})
	if !d.quit || d.m.ReloadBinary() != "/bin/new" {
		t.Fatalf("after ReloadMsg: quit %v, binary %q; want quit with /bin/new", d.quit, d.m.ReloadBinary())
	}
	if d.m.SelectedID() != want {
		t.Errorf("SelectedID() = %s, want %s", d.m.SelectedID(), want)
	}
}

// While an operation runs or a dialog is open, the TUI refuses the reload and keeps running.
func TestUpdate_ReloadRefusedWhileBusy(t *testing.T) {
	tests := []struct {
		name string
		busy func(d *dashboard)
		done func(d *dashboard) // what ends the busy state; then a reload goes through
	}{
		{"closing a session", func(d *dashboard) { d.key("x") }, func(d *dashboard) { d.send(sessionKilledMsg{}) }},
		{"the quit prompt", func(d *dashboard) { d.key("ctrl+c") }, func(d *dashboard) { d.key("esc") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDashboard(t, 3, 20)
			tt.busy(d)
			var refused error
			d.send(ReloadMsg{Binary: "/bin/new", Refuse: func(err error) { refused = err }})
			if d.quit || refused == nil {
				t.Fatalf("while busy: quit %v, refused %v; want refused", d.quit, refused)
			}
			tt.done(d)
			refused = nil
			d.send(ReloadMsg{Binary: "/bin/new", Refuse: func(err error) { refused = err }})
			if !d.quit || refused != nil {
				t.Errorf("after it ended: quit %v, refused %v; want quit", d.quit, refused)
			}
		})
	}
}

// A refused reload leaves the TUI running and says why.
func TestUpdate_ReloadFailed(t *testing.T) {
	d := newDashboard(t, 3, 20)
	d.send(ReloadFailedMsg{Err: errors.New("version: exit status 2")})
	if d.quit || d.m.ReloadBinary() != "" {
		t.Fatalf("after ReloadFailedMsg: quit %v, binary %q; want neither", d.quit, d.m.ReloadBinary())
	}
	if !strings.Contains(d.m.statusMsg, "exit status 2") {
		t.Errorf("status = %q, want the reason", d.m.statusMsg)
	}
}

// The TUI started again puts the cursor on the session it was on.
func TestWithSelection(t *testing.T) {
	d := newDashboard(t, 4, 20)
	target := d.mgr.sessions[1].ID
	m := d.m.WithSelection(target)
	if m.SelectedID() != target || m.cursor != 1 {
		t.Errorf("WithSelection: selected %s at %d, want %s at 1", m.SelectedID(), m.cursor, target)
	}
	// A session that is gone leaves the cursor where a fresh start puts it.
	m = d.m.WithSelection("gone")
	if m.cursor != 3 {
		t.Errorf("WithSelection(gone): cursor %d, want 3 (the newest)", m.cursor)
	}
}
