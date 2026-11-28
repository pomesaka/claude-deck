package main

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pomesaka/claude-deck/internal/session"
	"github.com/pomesaka/claude-deck/internal/store"
)

// The figures Claude Code did not measure are null, not 0, and hints is always an array.
func TestInfoFromSnapshot_Context(t *testing.T) {
	tests := []struct {
		name        string
		context     *session.ContextUsage
		compactions *int
		want        string
	}{
		{
			name:        "measured and full",
			context:     &session.ContextUsage{UsedTokens: 120_000, WindowTokens: 200_000},
			compactions: new(1),
			want:        `"context":{"used_tokens":120000,"window_tokens":200000},"compactions":1,"hints":[{"kind":"context_full","message":"文脈が詰まっている（使用率 60%、compaction 1 回）。きりのいい所で claude-deck clear --id abc を打つ"}]`,
		},
		{
			name:        "zero is a measurement",
			context:     &session.ContextUsage{UsedTokens: 0, WindowTokens: 200_000},
			compactions: new(0),
			want:        `"context":{"used_tokens":0,"window_tokens":200000},"compactions":0,"hints":[]`,
		},
		{
			name: "not measured",
			want: `"context":null,"compactions":null,"hints":[]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := session.Snapshot{ID: "abc", Origin: session.OriginDeck, Status: session.StatusIdle, Context: tt.context, Compactions: tt.compactions}
			out, err := json.Marshal(infoFromSnapshot(snap, session.Hints(snap, time.Time{}, nil)), jsontext.EscapeForHTML(false))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), tt.want) {
				t.Errorf("JSON = %s\nwant it to contain %s", out, tt.want)
			}
		})
	}
}

func openCLIStore(t *testing.T, recs ...store.Record) *store.Store {
	t.Helper()
	st, err := session.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, r := range recs {
		if err := st.Insert(r); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func listedHints(t *testing.T, st *store.Store, id string) []HintInfo {
	t.Helper()
	snaps, err := session.ListStored(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range snaps {
		if string(s.ID) == id {
			return infoFromSnapshot(s, session.Hints(s, time.Now(), nil)).Hints
		}
	}
	t.Fatalf("session %s not listed", id)
	return nil
}

// What the hooks write reaches list, and hints follow the latest figures: they are not stored.
func TestHooksReachList(t *testing.T) {
	st := openCLIStore(t, store.Record{ID: "abc", Name: "anna-8cc7", Status: "idle", SessionChain: []string{"c1"}, Isolation: store.IsolationDirect})
	if err := session.RecordContext(st, "abc", new(150_000), 200_000); err != nil {
		t.Fatal(err)
	}
	if got := listedHints(t, st, "abc"); len(got) != 1 || got[0].Kind != string(session.HintContextFull) {
		t.Errorf("after a full context: hints = %+v, want one context_full", got)
	}
	// /clear: back to no compaction and no fill.
	if err := session.RecordSessionStart(st, "abc", "c2", session.SourceClear); err != nil {
		t.Fatal(err)
	}
	if err := session.RecordContext(st, "abc", new(20_000), 200_000); err != nil {
		t.Fatal(err)
	}
	if got := listedHints(t, st, "abc"); len(got) != 0 {
		t.Errorf("after /clear and a low fill: hints = %+v, want none", got)
	}
}

// A session that rested for three days is stale in list until a hook reports it moving.
func TestHooksReachList_Stale(t *testing.T) {
	old := time.Now().Add(-72 * time.Hour)
	st := openCLIStore(t, store.Record{ID: "abc", Name: "anna-8cc7", Status: "idle", StartedAt: old, Isolation: store.IsolationDirect})
	if got := listedHints(t, st, "abc"); len(got) != 1 || got[0].Kind != string(session.HintStale) {
		t.Errorf("after three days: hints = %+v, want one stale", got)
	}
	if err := session.RecordHookStatus(st, "abc", session.ActivityRunning, session.Wait{}); err != nil {
		t.Fatal(err)
	}
	if err := session.RecordHookStatus(st, "abc", session.ActivityIdle, session.Wait{}); err != nil {
		t.Fatal(err)
	}
	if got := listedHints(t, st, "abc"); len(got) != 0 {
		t.Errorf("after a turn: hints = %+v, want none", got)
	}
}

// fakeWorkspaces answers WorkspaceChanges for listHints and records the paths read.
type fakeWorkspaces struct {
	files, commits int
	err            error
	read           []string
}

func (f *fakeWorkspaces) WorkspaceChanges(wsPath string) (int, int, error) {
	f.read = append(f.read, wsPath)
	return f.files, f.commits, f.err
}

// list reads a workspace only for a stale session that has one, and a failed read drops only unsaved.
func TestListHints_ReadsOnlyStaleWorkspaces(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	old, recent := new(now.Add(-72*time.Hour)), new(now.Add(-time.Hour))
	tests := []struct {
		name      string
		iso       session.Isolation
		activeAt  *time.Time
		ws        fakeWorkspaces
		wantRead  []string
		wantKinds []string
	}{
		{"stale with a workspace", session.WorkspaceIsolation("/ws/anna"), old, fakeWorkspaces{files: 1}, []string{"/ws/anna"}, []string{"stale", "unsaved"}},
		{"stale with a clean workspace", session.WorkspaceIsolation("/ws/anna"), old, fakeWorkspaces{}, []string{"/ws/anna"}, []string{"stale"}},
		{"recent with a workspace", session.WorkspaceIsolation("/ws/anna"), recent, fakeWorkspaces{files: 1}, nil, nil},
		{"stale without a workspace", session.DirectIsolation("/repo"), old, fakeWorkspaces{files: 1}, nil, []string{"stale"}},
		{"a workspace jj cannot read", session.WorkspaceIsolation("/ws/anna"), old, fakeWorkspaces{err: errors.New("jj failed")}, []string{"/ws/anna"}, []string{"stale"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := session.Snapshot{ID: "abc", Name: "anna", Origin: session.OriginDeck, Status: session.StatusIdle, Isolation: tt.iso, ActiveAt: tt.activeAt}
			var kinds []string
			for _, h := range listHints(s, now, &tt.ws) {
				kinds = append(kinds, string(h.Kind))
			}
			if !slices.Equal(tt.ws.read, tt.wantRead) || !slices.Equal(kinds, tt.wantKinds) {
				t.Errorf("read %v, hints %v; want read %v, hints %v", tt.ws.read, kinds, tt.wantRead, tt.wantKinds)
			}
		})
	}
}
