package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config holds the application configuration.
type Config struct {
	Defaults   DefaultConfig             `toml:"defaults"`
	Discovery  DiscoveryConfig           `toml:"discovery"`
	Ghostty    GhosttyConfig             `toml:"ghostty"`
	Keybinds   KeybindConfig             `toml:"keybinds"`
	Theme      ThemeConfig               `toml:"theme"`
	Commands   CommandsConfig            `toml:"commands"`
	Session    SessionConfig             `toml:"session"`
	Pricing    PricingConfig             `toml:"pricing"`
	Projects   map[string]ProjectConfig  `toml:"projects"`
	DataDir    string                    `toml:"data_dir"`
}

// DiscoveryConfig holds settings for repository and project discovery.
type DiscoveryConfig struct {
	// ProjectMarkers are filenames (e.g. "go.mod", "package.json") used to find
	// project directories within jj repositories. Empty means repo root only.
	ProjectMarkers []string `toml:"project_markers"`
	// Excludes are directory names to skip during fd search.
	Excludes       []string `toml:"excludes"`
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
	Border          string `toml:"border"`
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
	JJ     string `toml:"jj"`
}

// SessionConfig holds session management limits.
type SessionConfig struct {
	MaxSessions     int    `toml:"max_sessions"`
	MaxLogLines     int    `toml:"max_log_lines"`
	MaxScrollback   int    `toml:"max_scrollback"`
	MaxJSONLEntries int    `toml:"max_jsonl_entries"`
	DiscoveryDays   int    `toml:"discovery_days"`
	RefreshInterval string `toml:"refresh_interval"`
}

// PricingConfig holds token pricing per million tokens (USD).
type PricingConfig struct {
	InputPerMTok      float64 `toml:"input_per_mtok"`
	OutputPerMTok     float64 `toml:"output_per_mtok"`
	CacheWritePerMTok float64 `toml:"cache_write_per_mtok"`
	CacheReadPerMTok  float64 `toml:"cache_read_per_mtok"`
}

// DefaultConfig holds default settings.
type DefaultConfig struct {
	PermissionMode string `toml:"permission_mode"`
}

// GhosttyConfig holds Ghostty terminal settings.
type GhosttyConfig struct {
	Command string `toml:"command"`
}

// KeybindConfig allows overriding default keybindings.
type KeybindConfig struct {
	NewSession string `toml:"new_session"`
	Approve    string `toml:"approve"`
	Deny       string `toml:"deny"`
	Reply      string `toml:"reply"`
	Prompt     string `toml:"prompt"`
	OpenTerm   string `toml:"open_term"`
	Fork       string `toml:"fork"`
	Kill       string `toml:"kill"`
	Quit       string `toml:"quit"`
	Help       string `toml:"help"`
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
		Defaults: DefaultConfig{
			PermissionMode: "default",
		},
		Discovery: DiscoveryConfig{
			Excludes: []string{"Library", ".cache", "node_modules", ".git"},
		},
		Ghostty: GhosttyConfig{
			Command: "ghostty",
		},
		Keybinds: KeybindConfig{
			NewSession: "n",
			Approve:    "a",
			Deny:       "d",
			Reply:      "r",
			Prompt:     "p",
			OpenTerm:   "t",
			Fork:       "f",
			Kill:       "x",
			Quit:       "q",
			Help:       "?",
		},
		Theme: ThemeConfig{
			Primary:         "#7C3AED",
			Secondary:       "#06B6D4",
			Success:         "#10B981",
			Warning:         "#F59E0B",
			Danger:          "#EF4444",
			BgSelected:      "#313244",
			Border:          "#45475A",
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
			JJ:     "jj",
		},
		Session: SessionConfig{
			MaxSessions:     30,
			MaxLogLines:     1000,
			MaxScrollback:   2000,
			MaxJSONLEntries: 500,
			DiscoveryDays:   14,
			RefreshInterval: "5s",
		},
		Pricing: PricingConfig{
			InputPerMTok:      15.0,
			OutputPerMTok:     75.0,
			CacheWritePerMTok: 18.75,
			CacheReadPerMTok:  1.50,
		},
		DataDir: DefaultDataDir(),
	}
}

// Load reads configuration from the default config file.
func Load() (*Config, error) {
	return LoadFrom(filepath.Join(DefaultConfigDir(), "config.toml"))
}

// LoadFrom reads configuration from the specified path.
func LoadFrom(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if cfg.DataDir == "" {
		cfg.DataDir = DefaultDataDir()
	}

	return cfg, nil
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
