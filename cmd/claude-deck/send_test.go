package main

import (
	"errors"
	"slices"
	"testing"

	"github.com/pomesaka/claude-deck/internal/store"
	"github.com/pomesaka/claude-deck/internal/tmux"
)

func broadcastStore(t *testing.T) *store.Store {
	t.Helper()
	return openCLIStore(t,
		store.Record{ID: "a", Name: "anna", Status: "idle", RepoPath: "/src/claude-deck", Isolation: store.IsolationDirect},
		store.Record{ID: "b", Name: "bea", Status: "running", RepoPath: "/src/claude-deck", Isolation: store.IsolationDirect},
		store.Record{ID: "c", Name: "cleo", Status: "completed", RepoPath: "/src/claude-deck", Isolation: store.IsolationDirect},
		store.Record{ID: "d", Name: "dora", Status: "idle", RepoPath: "/src/dotclaude", Isolation: store.IsolationDirect},
		store.Record{ID: "e1", Name: "emi", Status: "idle", RepoPath: "/src/dotclaude", Isolation: store.IsolationDirect},
		store.Record{ID: "e2", Name: "emi", Status: "idle", RepoPath: "/src/dotclaude", Isolation: store.IsolationDirect},
	)
}

// A server that does not exist: a session that gets as far as reading its screen is skipped.
var noServer = &tmux.Runner{Socket: "cdtest-none", SessionName: "none"}

// broadcast takes the live sessions that pass the filter.
func TestBroadcast_Targets(t *testing.T) {
	st := broadcastStore(t)
	tests := []struct {
		name    string
		filter  sessionFilter
		wantIDs []string
	}{
		{"every live session", sessionFilter{}, []string{"a", "b", "d", "e1", "e2"}},
		{"one repository", sessionFilter{Repo: "deck"}, []string{"a", "b"}},
		{"named sessions", sessionFilter{Only: []string{"anna", "e2"}}, []string{"a", "e2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infos, err := broadcast(st, noServer, "", tt.filter, "/reload-plugins", true)
			if err != nil {
				t.Fatalf("broadcast() = %v", err)
			}
			var ids []string
			for _, in := range infos {
				ids = append(ids, in.ID)
				if in.Sent || in.SkippedReason == "" {
					t.Errorf("%s: sent %v, reason %q; want skipped with a reason (no window)", in.ID, in.Sent, in.SkippedReason)
				}
			}
			slices.Sort(ids) // in the order of the list, which this test does not check
			if !slices.Equal(ids, tt.wantIDs) {
				t.Errorf("targets = %v, want %v", ids, tt.wantIDs)
			}
		})
	}
}

// broadcast sends nothing when the filter names no target, or names one wrongly.
func TestBroadcast_Refuses(t *testing.T) {
	st := broadcastStore(t)
	tests := []struct {
		name          string
		filter        sessionFilter
		wantNoTargets bool
	}{
		{"a finished session is not a target", sessionFilter{Only: []string{"cleo"}}, true},
		{"a misspelt filter", sessionFilter{Repo: "dek"}, true},
		{"a misspelt name among --only", sessionFilter{Only: []string{"anna", "dorra"}}, false},
		{"a name two sessions have", sessionFilter{Only: []string{"emi"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infos, err := broadcast(st, noServer, "", tt.filter, "/reload-plugins", true)
			if err == nil || infos != nil {
				t.Fatalf("broadcast() = (%v, %v), want an error and nothing sent", infos, err)
			}
			if errors.Is(err, errNoTargets) != tt.wantNoTargets {
				t.Errorf("broadcast() = %v, want errNoTargets: %v", err, tt.wantNoTargets)
			}
		})
	}
}
