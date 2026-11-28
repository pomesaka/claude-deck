package main

import (
	"testing"

	"github.com/pomesaka/claude-deck/internal/session"
)

func TestAliasMatches(t *testing.T) {
	tests := []struct {
		alias, filter string
		want          bool
	}{
		{"review-pr-12", "", true},
		{"", "", true},
		{"review-pr-12", "review-pr-12", true},
		{"review-pr-12", "pr-12", true},
		{"review-pr-12", "Review", true},
		{"review-pr-12", "fix", false},
		{"", "review", false},
	}
	for _, tt := range tests {
		if got := aliasMatches(tt.alias, tt.filter); got != tt.want {
			t.Errorf("aliasMatches(%q, %q) = %v, want %v", tt.alias, tt.filter, got, tt.want)
		}
	}
}

func TestRepoMatches(t *testing.T) {
	tests := []struct {
		name, repoPath, filter string
		want                   bool
	}{
		{"no filter passes", "/src/github.com/pomesaka/claude-deck", "", true},
		{"no filter passes an empty path", "", "", true},
		{"whole name", "/src/github.com/pomesaka/claude-deck", "claude-deck", true},
		{"part of the name", "/src/github.com/pomesaka/claude-deck", "deck", true},
		{"case is ignored", "/src/github.com/pomesaka/claude-deck", "Deck", true},
		{"trailing slash", "/src/github.com/pomesaka/claude-deck/", "deck", true},
		{"a parent directory does not match", "/src/github.com/pomesaka/claude-deck", "pomesaka", false},
		{"another repository", "/src/github.com/pomesaka/dotclaude", "deck", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := repoMatches(tt.repoPath, tt.filter); got != tt.want {
				t.Errorf("repoMatches(%q, %q) = %v, want %v", tt.repoPath, tt.filter, got, tt.want)
			}
		})
	}
}

// Without --session, fork forks the calling session; --session wins over it.
func TestSessionFilter(t *testing.T) {
	s := session.Snapshot{ID: "abc123", Name: "anna-8cc7", Alias: "review-pr-12", RepoPath: "/src/claude-deck"}
	tests := []struct {
		name   string
		filter sessionFilter
		want   bool
	}{
		{"no condition", sessionFilter{}, true},
		{"only by name", sessionFilter{Only: []string{"anna-8cc7"}}, true},
		{"only by ID", sessionFilter{Only: []string{"x", "abc123"}}, true},
		{"only does not take an alias", sessionFilter{Only: []string{"review-pr-12"}}, false},
		{"only is exact", sessionFilter{Only: []string{"anna"}}, false},
		{"every condition holds", sessionFilter{Alias: "review", Repo: "deck", Only: []string{"anna-8cc7"}}, true},
		{"one condition fails", sessionFilter{Alias: "review", Repo: "dotclaude"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.filter.matches(s); got != tt.want {
				t.Errorf("matches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckOnly(t *testing.T) {
	targets := []session.Snapshot{{ID: "a1", Name: "anna"}, {ID: "b1", Name: "bea"}, {ID: "b2", Name: "bea"}}
	tests := []struct {
		name    string
		only    []string
		wantErr bool
	}{
		{"no names", nil, false},
		{"every name found", []string{"anna", "b1"}, false},
		{"a misspelt name", []string{"anna", "anan"}, true},
		{"a name two sessions have", []string{"bea"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkOnly(targets, tt.only); (err != nil) != tt.wantErr {
				t.Errorf("checkOnly(%v) = %v, want error %v", tt.only, err, tt.wantErr)
			}
		})
	}
}
