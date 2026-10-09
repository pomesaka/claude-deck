package tui

import (
	"slices"
	"testing"
)

func TestWithoutNestedRepos(t *testing.T) {
	tests := []struct {
		name      string
		root      string
		dirs      []string
		repoRoots []string
		want      []string
	}{
		{
			name:      "nested repo and its subdirs are dropped",
			root:      "/home/gh",
			dirs:      []string{"/home/gh", "/home/gh/app", "/home/gh/deck", "/home/gh/deck/tools"},
			repoRoots: []string{"/home/gh", "/home/gh/deck"},
			want:      []string{"/home/gh", "/home/gh/app"},
		},
		{
			name:      "sibling with a shared name prefix is kept",
			root:      "/home/gh",
			dirs:      []string{"/home/gh", "/home/gh/deck-ui"},
			repoRoots: []string{"/home/gh", "/home/gh/deck"},
			want:      []string{"/home/gh", "/home/gh/deck-ui"},
		},
		{
			name:      "repos outside root do not matter",
			root:      "/home/gh/deck",
			dirs:      []string{"/home/gh/deck", "/home/gh/deck/tools"},
			repoRoots: []string{"/home/gh", "/home/gh/deck", "/home/other"},
			want:      []string{"/home/gh/deck", "/home/gh/deck/tools"},
		},
		{
			name:      "no nested repos",
			root:      "/home/gh",
			dirs:      []string{"/home/gh", "/home/gh/app"},
			repoRoots: []string{"/home/gh"},
			want:      []string{"/home/gh", "/home/gh/app"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withoutNestedRepos(tt.root, tt.dirs, tt.repoRoots)
			if !slices.Equal(got, tt.want) {
				t.Errorf("withoutNestedRepos() = %v, want %v", got, tt.want)
			}
		})
	}
}
