package session

import (
	"slices"
	"testing"
	"time"
)

func TestHints_Stale(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { return new(now.Add(-d)) }
	ws := WorkspaceIsolation("/ws/anna-8cc7")
	removed := WorkspaceIsolation("/ws/anna-8cc7").Removed("at", "parent")
	tests := []struct {
		name      string
		status    Status
		iso       Isolation
		activeAt  *time.Time
		changes   *WorkspaceChanges
		wantKinds []HintKind
	}{
		{"idle for two days", StatusIdle, ws, ago(48 * time.Hour), nil, []HintKind{HintStale}},
		{"idle for just under two days", StatusIdle, ws, ago(48*time.Hour - time.Minute), nil, nil},
		{"waiting for an answer for three days", StatusWaitingAnswer, ws, ago(72 * time.Hour), nil, []HintKind{HintStale}},
		{"running for three days is not stale", StatusRunning, ws, ago(72 * time.Hour), nil, nil},
		{"subagents running for three days is not stale", StatusSubagentRunning, ws, ago(72 * time.Hour), nil, nil},
		{"a direct session idle for three days", StatusIdle, DirectIsolation("/repo"), ago(72 * time.Hour), nil, []HintKind{HintStale}},
		{"finished with its workspace kept", StatusCompleted, ws, ago(72 * time.Hour), nil, []HintKind{HintStale}},
		{"finished with its workspace removed has nothing to close", StatusCompleted, removed, ago(72 * time.Hour), nil, nil},
		{"no time known", StatusIdle, ws, nil, nil, nil},
		{"stale with uncommitted files", StatusIdle, ws, ago(72 * time.Hour), &WorkspaceChanges{Files: 2}, []HintKind{HintStale, HintUnsaved}},
		{"stale with commits not in trunk", StatusIdle, ws, ago(72 * time.Hour), &WorkspaceChanges{Commits: 1}, []HintKind{HintStale, HintUnsaved}},
		{"stale with nothing unsaved", StatusIdle, ws, ago(72 * time.Hour), &WorkspaceChanges{}, []HintKind{HintStale}},
		{"changes of a session that is not stale are not shown", StatusIdle, ws, ago(time.Hour), &WorkspaceChanges{Files: 2}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Snapshot{ID: "abc", Name: "anna-8cc7", Origin: OriginDeck, Status: tt.status, Isolation: tt.iso, ActiveAt: tt.activeAt}
			var got []HintKind
			for _, h := range Hints(s, now, tt.changes) {
				got = append(got, h.Kind)
			}
			if !slices.Equal(got, tt.wantKinds) {
				t.Errorf("Hints() kinds = %v, want %v", got, tt.wantKinds)
			}
		})
	}
}

func TestHints_StaleMessages(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	s := Snapshot{ID: "abc", Name: "anna-8cc7", Origin: OriginDeck, Status: StatusIdle, Isolation: WorkspaceIsolation("/ws"), ActiveAt: new(now.Add(-79 * time.Hour))}
	got := Hints(s, now, &WorkspaceChanges{Files: 2, Commits: 3})
	want := []Hint{
		{Kind: HintStale, Message: "3日動いていない。要らなければ claude-deck close abc"},
		{Kind: HintUnsaved, Message: "未コミットの変更が 2 ファイル、push していないコミットが 3 個ある。閉じる前に入れるか要らないかを決める"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Hints() =\n%+v\nwant\n%+v", got, want)
	}
}

// The last time a session moved is the latest of the times known for it.
func TestLastMoved(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	tests := []struct {
		name string
		snap Snapshot
		want time.Time
	}{
		{"nothing known", Snapshot{}, time.Time{}},
		{"started only", Snapshot{StartedAt: at(1)}, at(1)},
		{"a hook after the start", Snapshot{StartedAt: at(1), ActiveAt: new(at(5))}, at(5)},
		{"the transcript is newer than the last hook", Snapshot{StartedAt: at(1), ActiveAt: new(at(5)), LastActivity: at(6)}, at(6)},
		{"finished last", Snapshot{StartedAt: at(1), ActiveAt: new(at(5)), FinishedAt: new(at(7))}, at(7)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.snap.LastMoved(); !got.Equal(tt.want) {
				t.Errorf("LastMoved() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHints_UnsavedMessage(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		changes WorkspaceChanges
		want    string
	}{
		{"files only", WorkspaceChanges{Files: 1}, "未コミットの変更が 1 ファイルある。閉じる前に入れるか要らないかを決める"},
		{"commits only", WorkspaceChanges{Commits: 2}, "push していないコミットが 2 個ある。閉じる前に入れるか要らないかを決める"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Snapshot{ID: "abc", Origin: OriginDeck, Status: StatusIdle, Isolation: WorkspaceIsolation("/ws"), ActiveAt: new(now.Add(-72 * time.Hour))}
			hints := Hints(s, now, &tt.changes)
			if len(hints) != 2 || hints[1].Message != tt.want {
				t.Errorf("Hints() = %+v, want unsaved %q", hints, tt.want)
			}
		})
	}
}
