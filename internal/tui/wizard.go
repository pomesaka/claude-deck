package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/pomesaka/claude-deck/internal/config"
)

// repoItem carries the jj repository root and the project working directory.
// For monorepo subprojects, projectDir differs from repoPath.
type repoItem struct {
	repoPath   string // .jj のあるリポジトリルート
	projectDir string // claude を起動するディレクトリ（絶対パス）
}

func (r repoItem) Title() string       { return r.projectDir }
func (r repoItem) Description() string { return "" }
func (r repoItem) FilterValue() string { return r.projectDir }

// repoListMsg delivers discovered project paths to the TUI.
type repoListMsg struct {
	repos []repoItem
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

// discoverRepos finds jj repositories and optionally discovers subprojects
// within each repo using the configured project markers.
func discoverRepos(cfg *config.Config) func() tea.Msg {
	return func() tea.Msg {
		home, err := os.UserHomeDir()
		if err != nil {
			return repoListMsg{err: fmt.Errorf("home dir: %w", err)}
		}

		fdPath, err := findFd()
		if err != nil {
			return repoListMsg{err: err}
		}

		excludes := cfg.Discovery.Excludes
		if len(excludes) == 0 {
			excludes = []string{"Library", ".cache", "node_modules", ".git"}
		}

		// Step 1: .jj リポジトリルートを検出
		repoRoots, err := findJJRepos(fdPath, home, excludes)
		if err != nil {
			return repoListMsg{err: err}
		}

		// Step 2: project_markers が設定されていれば、各リポジトリ内でサブプロジェクト検出
		markers := cfg.Discovery.ProjectMarkers
		if len(markers) == 0 {
			// マーカー未設定 → リポジトリルートのみ（既存動作）
			items := make([]repoItem, len(repoRoots))
			for i, root := range repoRoots {
				items[i] = repoItem{repoPath: root, projectDir: root}
			}
			return repoListMsg{repos: items}
		}

		var items []repoItem
		for _, root := range repoRoots {
			projects, err := findProjectDirs(fdPath, root, markers, excludes)
			if err != nil {
				// サブプロジェクト検出失敗時はルートだけ追加
				items = append(items, repoItem{repoPath: root, projectDir: root})
				continue
			}
			for _, dir := range projects {
				items = append(items, repoItem{repoPath: root, projectDir: dir})
			}
		}
		return repoListMsg{repos: items}
	}
}

// findJJRepos discovers .jj repository roots under the given base directory.
func findJJRepos(fdPath, baseDir string, excludes []string) ([]string, error) {
	args := []string{"-HI", "-t", "d"}
	for _, ex := range excludes {
		args = append(args, "--exclude", ex)
	}
	args = append(args, "^\\.jj$", baseDir)

	cmd := exec.Command(fdPath, args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("fd (jj repos): %w", err)
	}

	var repos []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		repoPath := filepath.Dir(filepath.Clean(line))
		repos = append(repos, repoPath)
	}
	return repos, nil
}

// findProjectDirs discovers project directories within a repository by searching
// for marker files. Always includes the repo root itself.
func findProjectDirs(fdPath, repoRoot string, markers, excludes []string) ([]string, error) {
	// マーカーから正規表現を構築: ^(go\.mod|package\.json|...)$
	var escaped []string
	for _, m := range markers {
		escaped = append(escaped, regexp.QuoteMeta(m))
	}
	pattern := "^(" + strings.Join(escaped, "|") + ")$"

	args := []string{"-HI", "-t", "f"}
	for _, ex := range excludes {
		args = append(args, "--exclude", ex)
	}
	args = append(args, pattern, repoRoot)

	cmd := exec.Command(fdPath, args...)
	out, err := cmd.Output()
	if err != nil {
		// fd returns exit code 1 when no matches found
		return []string{repoRoot}, nil
	}

	seen := map[string]bool{repoRoot: true}
	dirs := []string{repoRoot}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		dir := filepath.Dir(filepath.Clean(line))
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// startNewSession switches to repo selector mode and begins async repo discovery.
func (m *Model) startNewSession() tea.Cmd {
	m.mode = viewSelectRepo
	m.statusMsg = "リポジトリを検索中..."

	return discoverRepos(m.config)
}

// selectRepo creates a session for the selected project directory.
func (m *Model) selectRepo(item repoItem, withWorkspace bool) tea.Cmd {
	m.mode = viewDashboard
	m.statusMsg = "セッション作成中..."

	mgr := m.manager
	ctx := m.ctx
	cols, _, rows := m.detailPaneMetrics()
	return func() tea.Msg {
		sess, err := mgr.CreateSession(ctx, item.repoPath, item.projectDir, withWorkspace, cols, rows)
		var id string
		if sess != nil {
			id = sess.ID
		}
		return sessionCreatedMsg{sessionID: id, err: err}
	}
}
