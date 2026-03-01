package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// repoItem implements list.DefaultItem for the repo fuzzy finder.
type repoItem string

func (r repoItem) Title() string       { return string(r) }
func (r repoItem) Description() string { return "" }
func (r repoItem) FilterValue() string { return string(r) }

// repoListMsg delivers discovered jj repo paths to the TUI.
type repoListMsg struct {
	repos []string
	err   error
}

// findFd resolves the path to the fd binary, checking PATH and common locations.
func findFd() (string, error) {
	if p, err := exec.LookPath("fd"); err == nil {
		return p, nil
	}
	// ~/.cargo/bin is often not in PATH for non-interactive shells
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	cargo := filepath.Join(home, ".cargo", "bin", "fd")
	if _, err := os.Stat(cargo); err == nil {
		return cargo, nil
	}
	return "", fmt.Errorf("fd not found in PATH or ~/.cargo/bin")
}

// discoverJJRepos runs fd to find jj repositories under the home directory.
func discoverJJRepos() tea.Msg {
	home, err := os.UserHomeDir()
	if err != nil {
		return repoListMsg{err: fmt.Errorf("home dir: %w", err)}
	}

	fdPath, err := findFd()
	if err != nil {
		return repoListMsg{err: err}
	}

	cmd := exec.Command(fdPath, "-HI", "-t", "d",
		"--exclude", "Library",
		"--exclude", ".cache",
		"--exclude", "node_modules",
		"--exclude", ".git",
		"^\\.jj$", home)
	out, err := cmd.Output()
	if err != nil {
		return repoListMsg{err: fmt.Errorf("fd: %w", err)}
	}

	var repos []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// fd returns paths like "/Users/foo/path/to/repo/.jj/"
		// Clean trailing slash, then Dir to strip the .jj component.
		repoPath := filepath.Dir(filepath.Clean(line))
		repos = append(repos, repoPath)
	}
	return repoListMsg{repos: repos}
}

// startNewSession switches to repo selector mode and begins async repo discovery.
func (m *Model) startNewSession() tea.Cmd {
	m.mode = viewSelectRepo
	m.statusMsg = "リポジトリを検索中..."

	return discoverJJRepos
}

// selectRepo creates a session for the selected repository path.
func (m *Model) selectRepo(repoPath string) tea.Cmd {
	m.mode = viewDashboard
	m.statusMsg = "セッション作成中..."

	mgr := m.manager
	ctx := m.ctx
	cols, _, rows := m.detailPaneMetrics()
	return func() tea.Msg {
		sess, err := mgr.CreateSession(ctx, repoPath, cols, rows)
		var id string
		if sess != nil {
			id = sess.ID
		}
		return sessionCreatedMsg{sessionID: id, err: err}
	}
}
