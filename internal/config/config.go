package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config holds the application configuration.
type Config struct {
	Defaults  DefaultConfig            `toml:"defaults"`
	Discovery DiscoveryConfig          `toml:"discovery"`
	Ghostty   GhosttyConfig            `toml:"ghostty"`
	Tmux      TmuxConfig               `toml:"tmux"`
	Keybinds  KeybindConfig            `toml:"keybinds"`
	Theme     ThemeConfig              `toml:"theme"`
	Commands  CommandsConfig           `toml:"commands"`
	Runtime   RuntimeConfig            `toml:"runtime"`
	Session   SessionConfig            `toml:"session"`
	Projects  map[string]ProjectConfig `toml:"projects"`
	DataDir   string                   `toml:"data_dir"`
}

// DiscoveryConfig holds settings for repository and project discovery.
type DiscoveryConfig struct {
	// ProjectMarkers are filenames (e.g. "go.mod", "package.json") used to find
	// project directories within jj repositories. Empty means repo root only.
	ProjectMarkers []string `toml:"project_markers"`
	// Excludes are directory names to skip during fd search.
	Excludes []string `toml:"excludes"`
}

// ProjectConfig holds per-project settings keyed by repository path.
type ProjectConfig struct {
	WorkspaceSymlinks []string `toml:"workspace_symlinks"`
	// AddDirs lists additional directories to pass as --add-dir to Claude Code.
	// Absolute paths are used as-is; relative paths are resolved from the repository root.
	AddDirs []string `toml:"add_dirs"`
}

// ThemeConfig holds UI color settings.
type ThemeConfig struct {
	Primary         string `toml:"primary"`
	Secondary       string `toml:"secondary"`
	Success         string `toml:"success"`
	Warning         string `toml:"warning"`
	Danger          string `toml:"danger"`
	BgSelected      string `toml:"bg_selected"`
	BorderFocus     string `toml:"border_focus"`
	Text            string `toml:"text"`
	TextDim         string `toml:"text_dim"`
	StatusIdle      string `toml:"status_idle"`
	StatusAttention string `toml:"status_attention"`
	StatusDone      string `toml:"status_done"`
	DiffAdd         string `toml:"diff_add"`
	DiffDel         string `toml:"diff_del"`
}

// CommandsConfig holds external command paths.
type CommandsConfig struct {
	Claude string `toml:"claude"`
	Codex  string `toml:"codex"`
	JJ     string `toml:"jj"`
}

// RuntimeConfig selects the agent CLI and transcript layout used by the app.
type RuntimeConfig struct {
	Provider string `toml:"provider"`
}

// SessionConfig holds session management limits.
type SessionConfig struct {
	MaxSessions     int    `toml:"max_sessions"`
	MaxJSONLEntries int    `toml:"max_jsonl_entries"`
	DiscoveryDays   int    `toml:"discovery_days"`
	RefreshInterval string `toml:"refresh_interval"`
}

// DefaultConfig holds default settings.
type DefaultConfig struct {
	// PermissionMode is passed to every session claude-deck starts
	// (--permission-mode for Claude Code, --ask-for-approval for Codex).
	// WHY 既定は空: 空ならフラグを付けず、ランタイム自身の設定（Claude Code なら
	// settings.json の permissions.defaultMode）に任せる。既定値を持つと、利用者が
	// ランタイムの側で選んだモードを毎回上書きする。
	PermissionMode string `toml:"permission_mode"`
}

// GhosttyConfig holds Ghostty terminal settings.
type GhosttyConfig struct {
	Command string `toml:"command"`
	// DeckWidth is the pixel width of the left (claude-deck list) pane when
	// using Ghostty split mode.  The right pane (tmux attach) gets the remainder.
	DeckWidth int `toml:"deck_width"`
}

// TmuxConfig holds settings for the tmux process management backend.
type TmuxConfig struct {
	// Command is the tmux binary path; defaults to "tmux".
	Command string `toml:"command"`
	// SessionName is the tmux session name; defaults to "claude-deck".
	SessionName string `toml:"session_name"`
}

// KeybindConfig overrides the keys that can be rebound. The other keys are fixed
// (internal/tui/keys.go).
type KeybindConfig struct {
	OpenTerm string `toml:"open_term"`
	Fork     string `toml:"fork"`
}

// DefaultConfigDir returns the default configuration directory.
func DefaultConfigDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "claude-deck")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "claude-deck")
}

// DefaultDataDir returns the default data directory.
func DefaultDataDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "claude-deck")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "claude-deck")
}

// DefaultConfig returns the default configuration.
func Default() *Config {
	return &Config{
		Discovery: DiscoveryConfig{
			Excludes: []string{"Library", ".cache", "node_modules", ".git"},
		},
		Ghostty: GhosttyConfig{
			Command:   "ghostty",
			DeckWidth: 400,
		},
		Keybinds: KeybindConfig{
			OpenTerm: "t",
			Fork:     "f",
		},
		Theme: ThemeConfig{
			Primary:         "#7C3AED",
			Secondary:       "#06B6D4",
			Success:         "#10B981",
			Warning:         "#F59E0B",
			Danger:          "#EF4444",
			BgSelected:      "#313244",
			BorderFocus:     "#7C3AED",
			Text:            "#CDD6F4",
			TextDim:         "#6C7086",
			StatusIdle:      "#808898",
			StatusAttention: "#C08552",
			StatusDone:      "#333346",
			DiffAdd:         "#A6E3A1",
			DiffDel:         "#F38BA8",
		},
		Commands: CommandsConfig{
			Claude: "claude",
			Codex:  "codex",
			JJ:     "jj",
		},
		Runtime: RuntimeConfig{
			Provider: "claude",
		},
		Session: SessionConfig{
			MaxSessions:     30,
			MaxJSONLEntries: 500,
			DiscoveryDays:   14,
			RefreshInterval: "5s",
		},
		DataDir: DefaultDataDir(),
	}
}

// EnvDataDir overrides data_dir when set. claude-deck sets it for every Claude Code
// session it starts, so hook commands run inside the session write to the same
// store as the process that started it.
const EnvDataDir = "CLAUDE_DECK_DATA_DIR"

// RuntimeProvider returns the normalized runtime provider name.
func (c *Config) RuntimeProvider() string {
	provider := strings.ToLower(strings.TrimSpace(c.Runtime.Provider))
	if provider == "" {
		return "claude"
	}
	return provider
}

// Load reads configuration from the default config file.
func Load() (*Config, error) {
	cfg, err := LoadFrom(filepath.Join(DefaultConfigDir(), "config.toml"))
	if err != nil {
		return nil, err
	}
	if dir := os.Getenv(EnvDataDir); dir != "" {
		cfg.DataDir = dir
	}
	return cfg, nil
}

// LoadFrom reads configuration from the specified path.
func LoadFrom(path string) (*Config, error) {
	cfg := Default()
	explicit := struct {
		DataDir string `toml:"data_dir"`
		Tmux    struct {
			SessionName string `toml:"session_name"`
		} `toml:"tmux"`
	}{}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			applyRuntimeScopedDefaults(cfg, explicit.DataDir != "", explicit.Tmux.SessionName != "")
			return cfg, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	if err := toml.Unmarshal(data, &explicit); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if cfg.DataDir == "" {
		cfg.DataDir = DefaultDataDir()
	}
	applyRuntimeScopedDefaults(cfg, explicit.DataDir != "", explicit.Tmux.SessionName != "")

	return cfg, nil
}

func applyRuntimeScopedDefaults(cfg *Config, explicitDataDir, explicitTmuxSession bool) {
	if cfg.RuntimeProvider() != "codex" {
		return
	}
	if !explicitDataDir && cfg.DataDir == DefaultDataDir() {
		cfg.DataDir += "-codex"
	}
	if !explicitTmuxSession && (cfg.Tmux.SessionName == "" || cfg.Tmux.SessionName == "claude-deck") {
		cfg.Tmux.SessionName = "claude-deck-codex"
	}
}

// WorkspaceSymlinks returns the list of extra symlink paths configured for the given repository.
// Returns nil if no project config exists for the path.
func (c *Config) WorkspaceSymlinks(repoPath string) []string {
	if c.Projects == nil {
		return nil
	}
	pc, ok := c.Projects[repoPath]
	if !ok {
		return nil
	}
	return pc.WorkspaceSymlinks
}

// ResolvedAddDirs returns the --add-dir paths for the given repository,
// resolving relative paths against repoPath.
// Returns nil if no add_dirs are configured for the project.
func (c *Config) ResolvedAddDirs(repoPath string) []string {
	if c.Projects == nil {
		return nil
	}
	pc, ok := c.Projects[repoPath]
	if !ok || len(pc.AddDirs) == 0 {
		return nil
	}
	resolved := make([]string, 0, len(pc.AddDirs))
	for _, d := range pc.AddDirs {
		if filepath.IsAbs(d) {
			resolved = append(resolved, d)
		} else {
			resolved = append(resolved, filepath.Join(repoPath, d))
		}
	}
	return resolved
}

// EnsureDataDir creates the data directory if it doesn't exist.
func (c *Config) EnsureDataDir() error {
	return os.MkdirAll(c.DataDir, 0o755)
}
