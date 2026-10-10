package session

import (
	"strings"
	"testing"

	"github.com/pomesaka/claude-deck/internal/store"
)

func TestSetAlias(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		alias     string
		wantErr   string // substring of the error, "" for success
		wantID    string
		wantAlias string
	}{
		{"by ID", "id-a", "review-pr-12", "", "id-a", "review-pr-12"},
		{"by name", "anna-8cc7", "review", "", "id-a", "review"},
		{"dots and underscores", "id-a", "v1.2_fix", "", "id-a", "v1.2_fix"},
		{"empty removes the alias", "id-b", "", "", "id-b", ""},
		{"unknown session", "nobody", "review", "session not found", "", ""},
		{"name shared by two sessions", "twin-0000", "review", "ambiguous", "", ""},
		{"Japanese", "id-a", "レビュー対応", "only ASCII", "", ""},
		{"space", "id-a", "review pr", "only ASCII", "", ""},
		{"more than one line", "id-a", "a\nb", "only ASCII", "", ""},
		{"too long", "id-a", strings.Repeat("a", 41), "limit is 40", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatalf("store.Open: %v", err)
			}
			defer st.Close()
			for _, r := range []store.Record{
				{ID: "id-a", Name: "anna-8cc7", Status: "idle"},
				{ID: "id-b", Name: "emiri-78fb", Status: "idle", Alias: "old"},
				{ID: "id-c", Name: "twin-0000", Status: "idle"},
				{ID: "id-d", Name: "twin-0000", Status: "idle"},
			} {
				if err := st.Insert(r); err != nil {
					t.Fatalf("Insert: %v", err)
				}
			}

			snap, err := SetAlias(st, tt.key, tt.alias)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetAlias: %v", err)
			}
			if string(snap.ID) != tt.wantID || snap.Alias != tt.wantAlias {
				t.Errorf("returned %s alias %q, want %s alias %q", snap.ID, snap.Alias, tt.wantID, tt.wantAlias)
			}
			if r, _ := st.Get(tt.wantID); r.Alias != tt.wantAlias {
				t.Errorf("stored alias = %q, want %q", r.Alias, tt.wantAlias)
			}
		})
	}
}

func TestSnapshot_DisplayName(t *testing.T) {
	tests := []struct {
		name, alias, want string
	}{
		{"anna-8cc7", "", "anna-8cc7"},
		{"anna-8cc7", "review-pr-12", "review-pr-12"},
	}
	for _, tt := range tests {
		if got := (Snapshot{Name: tt.name, Alias: tt.alias}).DisplayName(); got != tt.want {
			t.Errorf("DisplayName(name %q, alias %q) = %q, want %q", tt.name, tt.alias, got, tt.want)
		}
	}
}

// The TUI follows an alias another process set.
func TestReload_FollowsAlias(t *testing.T) {
	m, _ := newTestManager(t)
	sess := createPlainSession(t, m)

	if _, err := SetAlias(otherProcess(t, m), string(sess.ID), "review-pr-12"); err != nil {
		t.Fatalf("SetAlias: %v", err)
	}
	m.Reload()

	if got := sess.Snapshot().Alias; got != "review-pr-12" {
		t.Errorf("Alias after reload = %q, want review-pr-12", got)
	}
	if !sess.MatchesFilter("review") {
		t.Error("the filter does not match the alias")
	}
}
