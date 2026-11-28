package session

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/pomesaka/claude-deck/internal/store"
)

func TestApplyHookStatus(t *testing.T) {
	const r1, r2 = "gcloud auth login を実行してください", "PR を承認してください"
	tests := []struct {
		name        string
		from        string
		fromReason  string
		to          Activity
		reason      string
		wantStatus  string
		wantReason  string
		wantChanged bool
	}{
		{"idle to running", "idle", "", ActivityRunning, "", "running", "", true},
		{"running to waiting approval", "running", "", ActivityWaitingApproval, "", "waiting_approval", "", true},
		{"waiting answer to idle", "waiting_answer", "", ActivityIdle, "", "idle", "", true},
		{"running to subagent running", "running", "", ActivitySubagentRunning, "", "subagent_running", "", true},
		{"subagent running to idle", "subagent_running", "", ActivityIdle, "", "idle", "", true},
		{"same status is not a change", "running", "", ActivityRunning, "", "running", "", false},
		{"completed is not revived by a late hook", "completed", "", ActivityRunning, "", "completed", "", false},
		{"error is not revived by a late hook", "error", "", ActivityIdle, "", "error", "", false},
		{"unknown stored status is left alone", "bogus", "", ActivityRunning, "", "bogus", "", false},
		{"running to waiting action keeps the reason", "running", "", ActivityWaitingAction, r1, "waiting_action", r1, true},
		{"a new reason for the same wait is a change", "waiting_action", r1, ActivityWaitingAction, r2, "waiting_action", r2, true},
		{"the same wait and reason is not a change", "waiting_action", r1, ActivityWaitingAction, r1, "waiting_action", r1, false},
		{"leaving the wait for running clears the reason", "waiting_action", r1, ActivityRunning, "", "running", "", true},
		{"leaving the wait for idle clears the reason", "waiting_action", r1, ActivityIdle, "", "idle", "", true},
		{"a reason with another activity is dropped", "running", "", ActivityIdle, r1, "idle", "", true},
		{"completed is not made to wait", "completed", "", ActivityWaitingAction, r1, "completed", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := store.Record{ID: "s", Status: tt.from, StatusReason: tt.fromReason}
			if tt.fromReason != "" {
				r.WaitKind = string(WaitCommand)
			}
			changed := applyHookStatus(&r, tt.to, Wait{Kind: WaitCommand, Reason: tt.reason})
			if r.Status != tt.wantStatus || r.StatusReason != tt.wantReason || changed != tt.wantChanged {
				t.Errorf("got (%q, %q, %v), want (%q, %q, %v)", r.Status, r.StatusReason, changed, tt.wantStatus, tt.wantReason, tt.wantChanged)
			}
		})
	}
}

func TestApplyHookStatus_WaitKind(t *testing.T) {
	const reason = "API の誤りで止まりました。何か送ると再開します"
	tests := []struct {
		name        string
		from        string
		fromKind    string
		to          Activity
		wait        Wait
		wantKind    string
		wantChanged bool
	}{
		{"waiting for an API error keeps its kind", "running", "", ActivityWaitingAction, Wait{Kind: WaitAPIError, Reason: reason}, "api_error", true},
		{"a new kind for the same reason is a change", "waiting_action", "command", ActivityWaitingAction, Wait{Kind: WaitAPIError, Reason: reason}, "api_error", true},
		// A row an older binary wrote has the reason but no kind.
		{"a kind for a row that has none is a change", "waiting_action", "", ActivityWaitingAction, Wait{Kind: WaitAPIError, Reason: reason}, "api_error", true},
		{"the same kind and reason is not a change", "waiting_action", "api_error", ActivityWaitingAction, Wait{Kind: WaitAPIError, Reason: reason}, "api_error", false},
		// A session that runs an older plugin sends no kind.
		{"a wait without a kind is kept without one", "running", "", ActivityWaitingAction, Wait{Reason: reason}, "", true},
		{"leaving the wait clears the kind", "waiting_action", "api_error", ActivityRunning, Wait{}, "", true},
		{"a kind with another activity is dropped", "running", "", ActivityIdle, Wait{Kind: WaitCommand, Reason: reason}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := store.Record{ID: "s", Status: tt.from, StatusReason: reason, WaitKind: tt.fromKind}
			if tt.from != "waiting_action" {
				r.StatusReason = ""
			}
			changed := applyHookStatus(&r, tt.to, tt.wait)
			if r.WaitKind != tt.wantKind || changed != tt.wantChanged {
				t.Errorf("got (%q, %v), want (%q, %v)", r.WaitKind, changed, tt.wantKind, tt.wantChanged)
			}
		})
	}
}

// 守る決まり: status_reason は waiting_action の行にだけ入る。どの遷移の後でも破れない。
func TestStatusReasonOnlyWhileWaitingAction(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	activities := []Activity{ActivityRunning, ActivityIdle, ActivityWaitingApproval, ActivityWaitingAnswer, ActivitySubagentRunning, ActivityWaitingAction}
	const reason = "ログインしてください"
	for _, a := range activities {
		r := store.Record{ID: "s", Status: StatusWaitingAction.ID(), StatusReason: reason}
		applyHookStatus(&r, a, Wait{Kind: WaitCommand, Reason: reason})
		if r.Status != StatusWaitingAction.ID() && r.StatusReason != "" {
			t.Errorf("hook %v: status %q kept reason %q", a.Status(), r.Status, r.StatusReason)
		}
	}
	// The other transitions, each from a row it acts on. A finished row with a reason
	// was left by a binary older than this rule.
	steps := []struct {
		name string
		from Status
		step func(r *store.Record)
	}{
		{"exited", StatusWaitingAction, func(r *store.Record) { applyExited(r, nil, func(string) bool { return true }, now) }},
		{"error", StatusWaitingAction, func(r *store.Record) { setError(r, "boom", now) }},
		{"adopted", StatusWaitingAction, func(r *store.Record) { markAdopted(r, now) }},
		{"resume", StatusCompleted, func(r *store.Record) { _ = beginResume(r, now) }},
		{"revive", StatusCompleted, func(r *store.Record) { reviveForLiveWindow(r, 1) }},
		{"abort resume", StatusIdle, func(r *store.Record) { abortResume(r, now) }},
	}
	for _, tt := range steps {
		r := store.Record{ID: "s", Status: tt.from.ID(), StatusReason: reason}
		tt.step(&r)
		if r.Status == tt.from.ID() {
			t.Errorf("%s: status stayed %q; the step did not act", tt.name, r.Status)
		}
		if r.StatusReason != "" {
			t.Errorf("%s: status %q kept reason %q", tt.name, r.Status, r.StatusReason)
		}
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

func TestApplyContextOnSessionStart(t *testing.T) {
	tests := []struct {
		name            string
		chain           []string
		compactions     *int
		source          string
		wantCompactions *int
		wantTokens      bool // the fill is still known
	}{
		{"startup of a new session counts from 0", nil, nil, SourceStartup, new(0), false},
		{"startup of a known conversation keeps the count", []string{"a"}, new(2), SourceStartup, new(2), true},
		{"resume keeps the count and the fill", []string{"a"}, new(2), SourceResume, new(2), true},
		{"clear goes back to 0", []string{"a"}, new(3), SourceClear, new(0), false},
		{"clear of an unknown count goes to 0", []string{"a"}, nil, SourceClear, new(0), false},
		{"compact counts one more", []string{"a"}, new(1), SourceCompact, new(2), false},
		{"compact of an unknown count stays unknown", []string{"a"}, nil, SourceCompact, nil, false},
		{"fork does not know the source's count", nil, new(0), SourceFork, nil, false},
		{"an unknown source changes nothing", []string{"a"}, new(1), "other", new(1), true},
	}
	t.Run("a finished row is left as is", func(t *testing.T) {
		r := store.Record{ID: "s", Status: "completed", SessionChain: []string{"a"}, Compactions: new(1)}
		applyContextOnSessionStart(&r, SourceCompact)
		if *r.Compactions != 1 {
			t.Errorf("Compactions = %d, want 1", *r.Compactions)
		}
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := store.Record{ID: "s", Status: "idle", SessionChain: tt.chain, Compactions: tt.compactions,
				ContextTokens: new(150_000), ContextWindow: new(200_000)}
			applyContextOnSessionStart(&r, tt.source)
			if !reflect.DeepEqual(r.Compactions, tt.wantCompactions) {
				t.Errorf("Compactions = %v, want %v", deref(r.Compactions), deref(tt.wantCompactions))
			}
			if (r.ContextTokens != nil) != tt.wantTokens {
				t.Errorf("ContextTokens = %v, want known = %v", deref(r.ContextTokens), tt.wantTokens)
			}
			if r.ContextWindow == nil || *r.ContextWindow != 200_000 {
				t.Errorf("ContextWindow = %v, want 200000 kept", deref(r.ContextWindow))
			}
		})
	}
}

func TestApplyContextMeasured(t *testing.T) {
	tests := []struct {
		name       string
		before     *int
		tokens     *int
		window     int
		wantTokens *int
	}{
		{"first measurement", nil, new(1_000), 200_000, new(1_000)},
		{"a new measurement replaces the last", new(1_000), new(90_000), 200_000, new(90_000)},
		{"zero tokens is a measurement", nil, new(0), 200_000, new(0)},
		{"no fill yet in the live window", new(90_000), nil, 1_000_000, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := store.Record{ID: "s", Status: "idle", ContextTokens: tt.before}
			applyContextMeasured(&r, tt.tokens, tt.window)
			if !reflect.DeepEqual(r.ContextTokens, tt.wantTokens) || r.ContextWindow == nil || *r.ContextWindow != tt.window {
				t.Errorf("got (%v, %v), want (%v, %d)", deref(r.ContextTokens), deref(r.ContextWindow), deref(tt.wantTokens), tt.window)
			}
		})
	}
}

// deref prints a nullable figure in a failure message.
func deref(n *int) any {
	if n == nil {
		return nil
	}
	return *n
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
			name:           "a status this version does not know becomes completed: the process did end",
			rec:            store.Record{ID: "s", Status: "bogus", SessionChain: []string{"a"}},
			withLog:        []string{"a"},
			wantStatus:     "completed",
			wantFinishedAt: &now,
			wantChain:      []string{"a"},
		},
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
			others:         []store.Record{{ID: "other", Status: "completed", SessionChain: []string{"a"}}},
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
		name        string
		closingAt   *time.Time
		launchingAt *time.Time
		wantErr     error
	}{
		{"not closing", nil, nil, nil},
		{"another close in progress", &recent, nil, ErrClosing},
		{"stale close from a crashed process", &stale, nil, nil},
		{"launch in progress", nil, &recent, ErrLaunching},
		{"stale launch from a crashed process", nil, &stale, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := store.Record{ID: "s", Status: "running", ClosingAt: tt.closingAt, LaunchingAt: tt.launchingAt}
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
	now := time.Unix(1_700_000_000, 0)
	recentClose := now.Add(-time.Second)
	staleClose := now.Add(-closingTimeout - time.Second)
	tests := []struct {
		name    string
		rec     store.Record
		wantErr bool
	}{
		{"completed session resumes", store.Record{ID: "s", Status: "completed", FinishedAt: &now, PID: 9}, false},
		{"error session resumes", store.Record{ID: "s", Status: "error", ErrorMessage: "gone", PID: 9}, false},
		{"running session cannot resume", store.Record{ID: "s", Status: "running"}, true},
		{"idle session cannot resume", store.Record{ID: "s", Status: "idle"}, true},
		{"unknown status cannot resume", store.Record{ID: "s", Status: "bogus"}, true},
		{"session being closed cannot resume", store.Record{ID: "s", Status: "completed", ClosingAt: &recentClose}, true},
		{"stale close from a crashed process does not block resume", store.Record{ID: "s", Status: "completed", ClosingAt: &staleClose}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.rec
			err := beginResume(&r, now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if r.Status != "idle" || r.FinishedAt != nil || r.ErrorMessage != "" || r.PID != 0 || r.ClosingAt != nil ||
				r.LaunchingAt == nil || !r.LaunchingAt.Equal(now) {
				t.Errorf("after resume: %+v", r)
			}
		})
	}
}

func TestAbortResume(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := store.Record{ID: "s", Status: "idle", LaunchingAt: &now}
	abortResume(&r, now)
	if r.Status != "completed" || r.FinishedAt == nil || !r.FinishedAt.Equal(now) || r.LaunchingAt != nil {
		t.Errorf("after abortResume: %+v", r)
	}
}

func TestMarkAdopted(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := store.Record{ID: "s", Status: StatusUnknown.ID()} // an external session's row (recordLocked)
	markAdopted(&r, now)
	if r.Status != "completed" || r.FinishedAt == nil || !r.FinishedAt.Equal(now) {
		t.Errorf("after markAdopted: %+v", r)
	}
	if err := beginResume(&r, now); err != nil {
		t.Errorf("beginResume on an adopted row: %v", err)
	}
}

func TestReviveForLiveWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name        string
		rec         store.Record
		wantChanged bool
		wantStatus  string
		wantPID     int
	}{
		{"completed row is revived", store.Record{ID: "s", Status: "completed", FinishedAt: &now, ClosingAt: &now}, true, "idle", 11},
		{"error row is revived", store.Record{ID: "s", Status: "error", ErrorMessage: "gone", FinishedAt: &now}, true, "idle", 11},
		{"running row is left as is", store.Record{ID: "s", Status: "running", PID: 7}, false, "running", 7},
		{"idle row being launched is left as is", store.Record{ID: "s", Status: "idle", LaunchingAt: &now}, false, "idle", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.rec
			if got := reviveForLiveWindow(&r, 11); got != tt.wantChanged {
				t.Fatalf("changed = %v, want %v", got, tt.wantChanged)
			}
			if r.Status != tt.wantStatus || r.PID != tt.wantPID {
				t.Errorf("status=%q pid=%d, want %q %d", r.Status, r.PID, tt.wantStatus, tt.wantPID)
			}
			if tt.wantChanged && (r.FinishedAt != nil || r.ErrorMessage != "" || r.ClosingAt != nil || r.LaunchingAt != nil) {
				t.Errorf("revived row keeps finished fields: %+v", r)
			}
		})
	}
}

func TestRecordWorkspaceRemoved(t *testing.T) {
	tests := []struct {
		name               string
		atRev, parentRev   string
		wantAt, wantParent string
	}{
		{"revisions read", "at-new", "parent-new", "at-new", "parent-new"},
		{"revisions unreadable: the older pair is dropped", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := store.Record{
				ID: "s", WorkspaceName: "ws", WorkspacePath: "/data/ws",
				LastJJRevision: "at-old", LastJJParentRevision: "parent-old",
			}
			recordWorkspaceRemoved(&r, tt.atRev, tt.parentRev)
			if r.WorkspaceName != "" || r.WorkspacePath != "" {
				t.Errorf("workspace not cleared: %+v", r)
			}
			if r.LastJJRevision != tt.wantAt || r.LastJJParentRevision != tt.wantParent {
				t.Errorf("revisions = %q %q, want %q %q", r.LastJJRevision, r.LastJJParentRevision, tt.wantAt, tt.wantParent)
			}
		})
	}
}

// A row is marked exited for a missing window only while it is live: an ended
// row has nothing to end, and a status this version does not know is left alone.
func TestVanished(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		status string
		want   bool
	}{
		{"idle", true},
		{"running", true},
		{"completed", false},
		{"bogus", false},
	}
	for _, tt := range tests {
		if got := vanished(store.Record{ID: "s", Status: tt.status}, now); got != tt.want {
			t.Errorf("vanished(%s) = %v, want %v", tt.status, got, tt.want)
		}
	}
}
