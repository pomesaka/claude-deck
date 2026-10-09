package session

import (
	"slices"
	"testing"

	"github.com/pomesaka/claude-deck/internal/agentruntime"
)

func TestStartSpec_SharedFlags(t *testing.T) {
	tests := []struct {
		name      string
		sessName  string
		pluginDir string
		addDirs   []string
		want      []string
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			m.config.AddDirsFunc = func(string) []string { return tt.addDirs }
			m.config.PluginDir = tt.pluginDir

			got := m.startSpec(agentruntime.LaunchNew, "", "/repo", tt.sessName, "/repo").Args
			if !slices.Equal(got, tt.want) {
				t.Errorf("startSpec(%q).Args = %v, want %v", tt.sessName, got, tt.want)
			}
		})
	}
}
