package session

import (
	"slices"
	"testing"

	"github.com/pomesaka/claude-deck/internal/agentruntime"
)

func TestStartSpec_SharedFlags(t *testing.T) {
	tests := []struct {
		name        string
		sessName    string
		pluginDir   string
		addDirs     []string
		projectArgs []string
		want        []string
	}{
		{
			name:      "plugin dir between name and add-dirs",
			sessName:  "anna-8cc7",
			pluginDir: "/data/plugin",
			addDirs:   []string{"/shared"},
			want:      []string{"--name", "anna-8cc7", "--plugin-dir", "/data/plugin", "--add-dir", "/shared"},
		},
		{
			name:     "name only",
			sessName: "anna-8cc7",
			want:     []string{"--name", "anna-8cc7"},
		},
		{
			name:     "name with add-dirs",
			sessName: "anna-8cc7",
			addDirs:  []string{"/shared", "/docs"},
			want:     []string{"--name", "anna-8cc7", "--add-dir", "/shared", "--add-dir", "/docs"},
		},
		{
			name:    "empty name omits --name",
			addDirs: []string{"/shared"},
			want:    []string{"--add-dir", "/shared"},
		},
		{
			name: "empty name without add-dirs",
			want: nil,
		},
		{
			name:        "project args come last",
			sessName:    "anna-8cc7",
			pluginDir:   "/data/plugin",
			addDirs:     []string{"/shared"},
			projectArgs: []string{"--dangerously-load-development-channels", "server:xwb"},
			want:        []string{"--name", "anna-8cc7", "--plugin-dir", "/data/plugin", "--add-dir", "/shared", "--dangerously-load-development-channels", "server:xwb"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			m.config.AddDirsFunc = func(string) []string { return tt.addDirs }
			m.config.ProjectArgsFunc = func(string) []string { return tt.projectArgs }
			m.config.PluginDir = tt.pluginDir

			got := m.startSpec(agentruntime.LaunchNew, "", "/repo", tt.sessName, "/repo", "").Args
			if !slices.Equal(got, tt.want) {
				t.Errorf("startSpec(%q).Args = %v, want %v", tt.sessName, got, tt.want)
			}
		})
	}
}

func TestStartSpec_ProjectArgsInEveryLaunchMode(t *testing.T) {
	tests := []struct {
		name string
		mode agentruntime.LaunchMode
		want []string
	}{
		{name: "new", mode: agentruntime.LaunchNew, want: []string{"--name", "anna-8cc7", "--x"}},
		{name: "resume", mode: agentruntime.LaunchResume, want: []string{"--resume", "claude-1", "--name", "anna-8cc7", "--x"}},
		{name: "fork", mode: agentruntime.LaunchFork, want: []string{"--resume", "claude-1", "--fork-session", "--name", "anna-8cc7", "--x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			m.config.ProjectArgsFunc = func(repoPath string) []string {
				if repoPath != "/repo" {
					return nil
				}
				return []string{"--x"}
			}

			got := m.startSpec(tt.mode, "claude-1", "/ws", "anna-8cc7", "/repo", "").Args
			if !slices.Equal(got, tt.want) {
				t.Errorf("startSpec(%v).Args = %v, want %v", tt.mode, got, tt.want)
			}
		})
	}
}
