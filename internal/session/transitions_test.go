package session

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/pomesaka/claude-deck/internal/store"
)

func TestApplyHookStatus(t *testing.T) {
	tests := []struct {
		name        string
		from        string
		to          Status
		wantStatus  string
		wantChanged bool
	}{
		{"idle to running", "idle", StatusRunning, "running", true},
		{"running to waiting approval", "running", StatusWaitingApproval, "waiting_approval", true},
		{"waiting answer to idle", "waiting_answer", StatusIdle, "idle", true},
		{"same status is not a change", "running", StatusRunning, "running", false},
		{"completed is not revived by a late hook", "completed", StatusRunning, "completed", false},
		{"error is not revived by a late hook", "error", StatusIdle, "error", false},
		{"external session is left alone", "unmanaged", StatusRunning, "unmanaged", false},
		{"unknown stored status is left alone", "bogus", StatusRunning, "bogus", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := store.Record{ID: "s", Status: tt.from}
			changed := applyHookStatus(&r, tt.to)
			if r.Status != tt.wantStatus || changed != tt.wantChanged {
				t.Errorf("got (%q, %v), want (%q, %v)", r.Status, changed, tt.wantStatus, tt.wantChanged)
			}
		})
	}
}

func TestApplySessionStart(t *testing.T) {
	tests := []struct {
		name        string
		chain       []string
		claudeID    string
		source      string
		wantChain   []string
		wantChanged bool
	}{
		{"startup sets the first ID", nil, "a", SourceStartup, []string{"a"}, true},
		{"resume sets the first ID", nil, "a", SourceResume, []string{"a"}, true},
		{"fork sets the first ID", nil, "a", SourceFork, []string{"a"}, true},
		{"startup with an existing chain is ignored", []string{"a"}, "b", SourceStartup, []string{"a"}, false},
		{"resume with an existing chain is ignored", []string{"a"}, "b", SourceResume, []string{"a"}, false},
		{"clear appends the new ID", []string{"a"}, "b", SourceClear, []string{"a", "b"}, true},
		{"compact appends the new ID", []string{"a"}, "b", SourceCompact, []string{"a", "b"}, true},
		{"clear on an empty chain appends", nil, "b", SourceClear, []string{"b"}, true},
		{"clear repeating the current ID is ignored", []string{"a", "b"}, "b", SourceClear, []string{"a", "b"}, false},
		{"empty ID is ignored", []string{"a"}, "", SourceClear, []string{"a"}, false},
		{"unknown source is ignored", []string{"a"}, "b", "other", []string{"a"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := store.Record{ID: "s", Status: "idle", SessionChain: tt.chain}
			changed := applySessionStart(&r, tt.claudeID, tt.source)
			if !reflect.DeepEqual(r.SessionChain, tt.wantChain) || changed != tt.wantChanged {
				t.Errorf("got (%v, %v), want (%v, %v)", r.SessionChain, changed, tt.wantChain, tt.wantChanged)
			}
		})
	}
}

func TestApplyExited(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	earlier := now.Add(-time.Hour)
	tests := []struct {
		name           string
		rec            store.Record
		others         []store.Record
		withLog        []string // Claude session IDs that have a conversation
		wantStatus     string
		wantFinishedAt *time.Time
		wantChain      []string
	}{
		{
			name:           "running session becomes completed",
			rec:            store.Record{ID: "s", Status: "running", SessionChain: []string{"a"}},
			withLog:        []string{"a"},
			wantStatus:     "completed",
			wantFinishedAt: &now,
			wantChain:      []string{"a"},
		},
		{
			name:           "already completed keeps its finish time",
			rec:            store.Record{ID: "s", Status: "completed", FinishedAt: &earlier, SessionChain: []string{"a"}},
			withLog:        []string{"a"},
			wantStatus:     "completed",
			wantFinishedAt: &earlier,
			wantChain:      []string{"a"},
		},
		{
			name:           "error stays error",
			rec:            store.Record{ID: "s", Status: "error", FinishedAt: &earlier},
			wantStatus:     "error",
			wantFinishedAt: &earlier,
		},
		{
			name:           "clear then exit without a message falls back to the previous ID",
			rec:            store.Record{ID: "s", Status: "idle", SessionChain: []string{"a", "b"}},
			withLog:        []string{"a"},
			wantStatus:     "completed",
			wantFinishedAt: &now,
			wantChain:      []string{"a"},
		},
		{
			name:           "newest ID with a conversation is kept",
			rec:            store.Record{ID: "s", Status: "idle", SessionChain: []string{"a", "b"}},
			withLog:        []string{"a", "b"},
			wantStatus:     "completed",
			wantFinishedAt: &now,
			wantChain:      []string{"a", "b"},
		},
		{
			name:           "no fallback when another session owns the previous ID",
			rec:            store.Record{ID: "s", Status: "idle", SessionChain: []string{"a", "b"}},
			others:         []store.Record{{ID: "other", Status: "unmanaged", SessionChain: []string{"a"}}},
			withLog:        []string{"a"},
			wantStatus:     "completed",
			wantFinishedAt: &now,
			wantChain:      []string{"a", "b"},
		},
		{
			name:           "the session itself in others does not block the fallback",
			rec:            store.Record{ID: "s", Status: "idle", SessionChain: []string{"a", "b"}},
			others:         []store.Record{{ID: "s", Status: "idle", SessionChain: []string{"a"}}},
			withLog:        []string{"a"},
			wantStatus:     "completed",
			wantFinishedAt: &now,
			wantChain:      []string{"a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hasConversation := func(id string) bool {
				for _, l := range tt.withLog {
					if l == id {
						return true
					}
				}
				return false
			}
			r := tt.rec
			applyExited(&r, tt.others, hasConversation, now)
			if r.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", r.Status, tt.wantStatus)
			}
			if !reflect.DeepEqual(r.FinishedAt, tt.wantFinishedAt) {
				t.Errorf("FinishedAt = %v, want %v", r.FinishedAt, tt.wantFinishedAt)
			}
			if !reflect.DeepEqual(r.SessionChain, tt.wantChain) {
				t.Errorf("SessionChain = %v, want %v", r.SessionChain, tt.wantChain)
			}
		})
	}
}

func TestBeginClose(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	recent := now.Add(-time.Second)
	stale := now.Add(-closingTimeout - time.Second)
	tests := []struct {
		name      string
		closingAt *time.Time
		wantErr   error
	}{
		{"not closing", nil, nil},
		{"another close in progress", &recent, ErrClosing},
		{"stale close from a crashed process", &stale, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := store.Record{ID: "s", Status: "running", ClosingAt: tt.closingAt}
			err := beginClose(&r, now)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if err == nil && (r.ClosingAt == nil || !r.ClosingAt.Equal(now)) {
				t.Errorf("ClosingAt = %v, want %v", r.ClosingAt, now)
			}
		})
	}
}

func TestBeginResume(t *testing.T) {
	finished := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name    string
		rec     store.Record
		wantErr bool
	}{
		{"completed session resumes", store.Record{ID: "s", Status: "completed", FinishedAt: &finished, PID: 9}, false},
		{"error session resumes", store.Record{ID: "s", Status: "error", ErrorMessage: "gone", PID: 9}, false},
		{"running session cannot resume", store.Record{ID: "s", Status: "running"}, true},
		{"idle session cannot resume", store.Record{ID: "s", Status: "idle"}, true},
		{"session being closed cannot resume", store.Record{ID: "s", Status: "completed", ClosingAt: &finished}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.rec
			err := beginResume(&r)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if r.Status != "idle" || r.FinishedAt != nil || r.ErrorMessage != "" || r.PID != 0 {
				t.Errorf("after resume: %+v", r)
			}
		})
	}
}
