package session

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/pomesaka/claude-deck/internal/store"
)

func TestSortSessions_Pinned(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	type in struct {
		name   string
		status Status
		pinned bool
		ago    time.Duration
	}
	tests := []struct {
		name     string
		sessions []in
		want     []string
	}{
		{
			name: "no pins keeps the status order",
			sessions: []in{
				{"approve", StatusWaitingApproval, false, 0},
				{"idle", StatusIdle, false, 0},
				{"done", StatusCompleted, false, 0},
			},
			want: []string{"done", "idle", "approve"},
		},
		{
			name: "a pinned finished session goes below an unpinned approval",
			sessions: []in{
				{"approve", StatusWaitingApproval, false, 0},
				{"pinned-done", StatusCompleted, true, 0},
				{"idle", StatusIdle, false, 0},
			},
			want: []string{"idle", "approve", "pinned-done"},
		},
		{
			name: "pinned sessions keep the status and time order among themselves",
			sessions: []in{
				{"pinned-approve", StatusWaitingApproval, true, 0},
				{"pinned-idle-new", StatusIdle, true, time.Minute},
				{"pinned-idle-old", StatusIdle, true, time.Hour},
				{"done", StatusCompleted, false, 0},
			},
			want: []string{"done", "pinned-idle-old", "pinned-idle-new", "pinned-approve"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			list := make([]*Session, len(tt.sessions))
			for i, s := range tt.sessions {
				list[i] = &Session{Name: s.name, Status: s.status, Pinned: s.pinned, LastActivity: base.Add(-s.ago)}
			}
			sortSessions(list)
			got := make([]string, len(list))
			for i, s := range list {
				got[i] = s.Name
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("order = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManager_SetPinned(t *testing.T) {
	m, _ := newTestManager(t)
	sess := createPlainSession(t, m)

	for _, pinned := range []bool{true, false} {
		if err := m.SetPinned(sess.ID, pinned); err != nil {
			t.Fatalf("SetPinned(%v): %v", pinned, err)
		}
		if got := mustGet(t, m.store, sess.ID).Pinned; got != pinned {
			t.Errorf("stored Pinned = %v, want %v", got, pinned)
		}
		m.Reload()
		if got := sess.Snapshot().Pinned; got != pinned {
			t.Errorf("Pinned after reload = %v, want %v", got, pinned)
		}
	}
}

// An external session is not in the store, so it cannot be pinned.
func TestManager_SetPinned_External(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.SetPinned("not-in-store", true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetPinned(external) error = %v, want store.ErrNotFound", err)
	}
}

// External sessions and rows with an unknown status have no process, so they
// sort with the finished ones at the top, however recent they are.
func TestSortSessions_NoProcessOnTop(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	mk := func(name string, origin Origin, status Status, ago time.Duration) *Session {
		return &Session{Name: name, Origin: origin, Status: status, LastActivity: base.Add(-ago)}
	}
	list := []*Session{
		mk("approve", OriginDeck, StatusWaitingApproval, time.Hour),
		mk("external", OriginExternal, StatusUnknown, 0),
		mk("idle", OriginDeck, StatusIdle, time.Hour),
		mk("unknown", OriginDeck, StatusUnknown, time.Minute),
		mk("done", OriginDeck, StatusCompleted, 2*time.Hour),
	}
	sortSessions(list)
	var got []string
	for _, s := range list {
		got = append(got, s.Name)
	}
	if want := []string{"done", "unknown", "external", "idle", "approve"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}
