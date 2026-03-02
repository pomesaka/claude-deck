package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	cfg := Default()
	if cfg.Defaults.PermissionMode != "default" {
		t.Errorf("PermissionMode = %q, want 'default'", cfg.Defaults.PermissionMode)
	}
	if cfg.Ghostty.Command != "ghostty" {
		t.Errorf("Ghostty.Command = %q, want 'ghostty'", cfg.Ghostty.Command)
	}
	if cfg.DataDir == "" {
		t.Error("expected non-empty DataDir")
	}
	if cfg.Keybinds.Quit != "q" {
		t.Errorf("Keybinds.Quit = %q, want 'q'", cfg.Keybinds.Quit)
	}
	// Theme defaults
	if cfg.Theme.Primary != "#7C3AED" {
		t.Errorf("Theme.Primary = %q, want '#7C3AED'", cfg.Theme.Primary)
	}
	if cfg.Theme.Text != "#CDD6F4" {
		t.Errorf("Theme.Text = %q, want '#CDD6F4'", cfg.Theme.Text)
	}
	// Commands defaults
	if cfg.Commands.Claude != "claude" {
		t.Errorf("Commands.Claude = %q, want 'claude'", cfg.Commands.Claude)
	}
	if cfg.Commands.JJ != "jj" {
		t.Errorf("Commands.JJ = %q, want 'jj'", cfg.Commands.JJ)
	}
	// Session defaults
	if cfg.Session.MaxSessions != 30 {
		t.Errorf("Session.MaxSessions = %d, want 30", cfg.Session.MaxSessions)
	}
	if cfg.Session.MaxLogLines != 1000 {
		t.Errorf("Session.MaxLogLines = %d, want 1000", cfg.Session.MaxLogLines)
	}
	if cfg.Session.MaxScrollback != 2000 {
		t.Errorf("Session.MaxScrollback = %d, want 2000", cfg.Session.MaxScrollback)
	}
	if cfg.Session.MaxJSONLEntries != 500 {
		t.Errorf("Session.MaxJSONLEntries = %d, want 500", cfg.Session.MaxJSONLEntries)
	}
	if cfg.Session.DiscoveryDays != 14 {
		t.Errorf("Session.DiscoveryDays = %d, want 14", cfg.Session.DiscoveryDays)
	}
	if cfg.Session.RefreshInterval != "5s" {
		t.Errorf("Session.RefreshInterval = %q, want '5s'", cfg.Session.RefreshInterval)
	}
	// Pricing defaults
	if cfg.Pricing.InputPerMTok != 15.0 {
		t.Errorf("Pricing.InputPerMTok = %f, want 15.0", cfg.Pricing.InputPerMTok)
	}
	if cfg.Pricing.OutputPerMTok != 75.0 {
		t.Errorf("Pricing.OutputPerMTok = %f, want 75.0", cfg.Pricing.OutputPerMTok)
	}
}

func TestLoadFrom_NonExistent(t *testing.T) {
	cfg, err := LoadFrom("/nonexistent/config.toml")
	if err != nil {
		t.Fatalf("expected no error for non-existent file, got: %v", err)
	}
	// Should return defaults
	if cfg.Defaults.PermissionMode != "default" {
		t.Errorf("expected default PermissionMode, got %q", cfg.Defaults.PermissionMode)
	}
}

func TestLoadFrom_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	content := `
[defaults]
permission_mode = "plan"

[ghostty]
command = "/usr/bin/ghostty"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom error: %v", err)
	}

	if cfg.Defaults.PermissionMode != "plan" {
		t.Errorf("PermissionMode = %q, want 'plan'", cfg.Defaults.PermissionMode)
	}
	if cfg.Ghostty.Command != "/usr/bin/ghostty" {
		t.Errorf("Ghostty.Command = %q", cfg.Ghostty.Command)
	}
}

func TestLoadFrom_NewConfigSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	content := `
[theme]
primary = "#FF0000"
text = "#FFFFFF"

[commands]
claude = "/usr/local/bin/claude"
jj = "/opt/bin/jj"

[session]
max_sessions = 50
max_log_lines = 2000
discovery_days = 7
refresh_interval = "10s"

[pricing]
input_per_mtok = 10.0
output_per_mtok = 50.0
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom error: %v", err)
	}

	if cfg.Theme.Primary != "#FF0000" {
		t.Errorf("Theme.Primary = %q, want '#FF0000'", cfg.Theme.Primary)
	}
	if cfg.Theme.Text != "#FFFFFF" {
		t.Errorf("Theme.Text = %q, want '#FFFFFF'", cfg.Theme.Text)
	}
	// Unset theme fields should keep defaults
	if cfg.Theme.Secondary != "#06B6D4" {
		t.Errorf("Theme.Secondary = %q, want default '#06B6D4'", cfg.Theme.Secondary)
	}
	if cfg.Commands.Claude != "/usr/local/bin/claude" {
		t.Errorf("Commands.Claude = %q", cfg.Commands.Claude)
	}
	if cfg.Commands.JJ != "/opt/bin/jj" {
		t.Errorf("Commands.JJ = %q", cfg.Commands.JJ)
	}
	if cfg.Session.MaxSessions != 50 {
		t.Errorf("Session.MaxSessions = %d, want 50", cfg.Session.MaxSessions)
	}
	if cfg.Session.MaxLogLines != 2000 {
		t.Errorf("Session.MaxLogLines = %d, want 2000", cfg.Session.MaxLogLines)
	}
	if cfg.Session.DiscoveryDays != 7 {
		t.Errorf("Session.DiscoveryDays = %d, want 7", cfg.Session.DiscoveryDays)
	}
	if cfg.Session.RefreshInterval != "10s" {
		t.Errorf("Session.RefreshInterval = %q, want '10s'", cfg.Session.RefreshInterval)
	}
	// Unset session fields should keep defaults
	if cfg.Session.MaxScrollback != 2000 {
		t.Errorf("Session.MaxScrollback = %d, want default 2000", cfg.Session.MaxScrollback)
	}
	if cfg.Session.MaxJSONLEntries != 500 {
		t.Errorf("Session.MaxJSONLEntries = %d, want default 500", cfg.Session.MaxJSONLEntries)
	}
	if cfg.Pricing.InputPerMTok != 10.0 {
		t.Errorf("Pricing.InputPerMTok = %f, want 10.0", cfg.Pricing.InputPerMTok)
	}
	if cfg.Pricing.OutputPerMTok != 50.0 {
		t.Errorf("Pricing.OutputPerMTok = %f, want 50.0", cfg.Pricing.OutputPerMTok)
	}
	// Unset pricing fields should keep defaults
	if cfg.Pricing.CacheWritePerMTok != 18.75 {
		t.Errorf("Pricing.CacheWritePerMTok = %f, want default 18.75", cfg.Pricing.CacheWritePerMTok)
	}
}

func TestLoadFrom_InvalidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")

	if err := os.WriteFile(path, []byte("this is not valid toml [[["), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadFrom(path)
	if err == nil {
		t.Error("expected error for invalid TOML")
	}
}

func TestEnsureDataDir(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.DataDir = filepath.Join(dir, "sub", "data")

	if err := cfg.EnsureDataDir(); err != nil {
		t.Fatalf("EnsureDataDir error: %v", err)
	}

	info, err := os.Stat(cfg.DataDir)
	if err != nil {
		t.Fatalf("DataDir not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("DataDir is not a directory")
	}
}

func TestLoadFrom_ProjectsConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	content := `
[projects."/Users/foo/myrepo"]
workspace_symlinks = [".env", ".env.local", "secrets/"]

[projects."/Users/foo/other-repo"]
workspace_symlinks = [".env"]
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom error: %v", err)
	}

	if len(cfg.Projects) != 2 {
		t.Fatalf("Projects count = %d, want 2", len(cfg.Projects))
	}

	pc, ok := cfg.Projects["/Users/foo/myrepo"]
	if !ok {
		t.Fatal("project /Users/foo/myrepo not found")
	}
	want := []string{".env", ".env.local", "secrets/"}
	if len(pc.WorkspaceSymlinks) != len(want) {
		t.Fatalf("WorkspaceSymlinks = %v, want %v", pc.WorkspaceSymlinks, want)
	}
	for i, w := range want {
		if pc.WorkspaceSymlinks[i] != w {
			t.Errorf("WorkspaceSymlinks[%d] = %q, want %q", i, pc.WorkspaceSymlinks[i], w)
		}
	}
}

func TestWorkspaceSymlinks(t *testing.T) {
	cfg := Default()
	cfg.Projects = map[string]ProjectConfig{
		"/repo/a": {WorkspaceSymlinks: []string{".env", ".env.local"}},
	}

	// 一致するパス
	got := cfg.WorkspaceSymlinks("/repo/a")
	if len(got) != 2 || got[0] != ".env" || got[1] != ".env.local" {
		t.Errorf("WorkspaceSymlinks(/repo/a) = %v, want [.env .env.local]", got)
	}

	// 一致しないパス
	if got := cfg.WorkspaceSymlinks("/repo/b"); got != nil {
		t.Errorf("WorkspaceSymlinks(/repo/b) = %v, want nil", got)
	}

	// Projects が nil のデフォルト
	cfg2 := Default()
	if got := cfg2.WorkspaceSymlinks("/repo/a"); got != nil {
		t.Errorf("WorkspaceSymlinks on default config = %v, want nil", got)
	}
}

func TestDefaultConfigDir(t *testing.T) {
	dir := DefaultConfigDir()
	if dir == "" {
		t.Error("expected non-empty config dir")
	}
}

func TestDefaultDataDir(t *testing.T) {
	dir := DefaultDataDir()
	if dir == "" {
		t.Error("expected non-empty data dir")
	}
}
