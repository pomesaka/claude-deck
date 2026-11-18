package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"

	"github.com/pomesaka/claude-deck/internal/config"
	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/session"
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

		// DataDir 配下のワークスペースが jj リポジトリとして検出されるのを防ぐ
		if rel, err := filepath.Rel(home, cfg.DataDir); err == nil && !strings.HasPrefix(rel, "..") {
			excludes = append(append([]string{}, excludes...), rel)
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
			for _, dir := range withoutNestedRepos(root, projects, repoRoots) {
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

// newRepoList builds the selector list. It is always used in the filtering
// state, so Enter/Esc are handled by handleRepoSelectKey and the list's own
// filter keys are disabled.
func newRepoList() list.Model {
	rl := list.New(nil, newRepoDelegate(), 80, 24)
	rl.Title = "リポジトリ選択"
	rl.SetShowStatusBar(true)
	rl.SetFilteringEnabled(true)
	rl.SetStatusBarItemName("repo", "repos")
	rl.DisableQuitKeybindings()
	rl.KeyMap.AcceptWhileFiltering.SetEnabled(false)
	rl.KeyMap.CancelWhileFiltering.SetEnabled(false)
	rl.KeyMap.Filter.SetEnabled(false)
	return rl
}

// startNewSession switches to repo selector mode, shows the last discovery
// result at once, and rediscovers in the background so that repositories added
// since then appear without restarting the TUI.
func (m *Model) startNewSession() tea.Cmd {
	m.mode = viewSelectRepo
	m.nextRightPaneGeneration()
	m.setRepoCandidates("", "")
	m.statusMsg = repoDiscoveringStatus

	if m.repoDiscovering {
		return nil
	}
	m.repoDiscovering = true
	return m.discoverRepos
}

const repoDiscoveringStatus = "リポジトリを更新中..."

// applyRepoList stores a discovery result and, if the selector is open, swaps
// it in while keeping what the user has typed and selected.
func (m *Model) applyRepoList(msg repoListMsg) tea.Cmd {
	m.repoDiscovering = false
	open := m.mode == viewSelectRepo
	// Esc で閉じた後も「更新中」が残らないよう、開いているかに関係なく消す。
	if m.statusMsg == repoDiscoveringStatus {
		m.statusMsg = ""
	}

	if msg.err != nil {
		if !open {
			// 画面は変えない。次の n で探索し直し、失敗すればそこで表示する。
			debuglog.Printf("[repoDiscovery] %v", msg.err)
			return nil
		}
		m.statusMsg = "リポジトリ検索エラー: " + msg.err.Error()
		if len(m.repoItems) == 0 {
			m.mode = viewDashboard
		}
		return clearStatusCmd()
	}

	// WHY: fd walks directories in parallel, so its output order changes per run.
	// Sorting keeps the list from reshuffling when a fresh result replaces the
	// shown one under the user's cursor.
	m.repoItems = slices.SortedFunc(slices.Values(msg.repos), func(a, b repoItem) int {
		return strings.Compare(a.projectDir, b.projectDir)
	})
	if !open {
		return nil
	}
	var selected string
	if ri, ok := m.repoList.SelectedItem().(repoItem); ok {
		selected = ri.projectDir
	}
	m.setRepoCandidates(m.repoList.FilterValue(), selected)
	return nil
}

// setRepoCandidates puts m.repoItems into the selector in the filtering state,
// with filter as the input text and selectedDir selected when it is visible.
func (m *Model) setRepoCandidates(filter, selectedDir string) {
	items := make([]list.Item, len(m.repoItems))
	for i, r := range m.repoItems {
		items[i] = r
	}
	// SetItems はフィルタ中だと絞り込みを非同期の Cmd で返すので、
	// 先に Unfiltered に戻してから入れ、SetFilterText で同期的に絞り込む。
	// SetFilterState(Filtering) は入力欄にフォーカスを戻すために呼ぶ
	// （SetFilterState 単体では filteredItems が空のままになる）。
	m.repoList.ResetFilter()
	m.repoList.SetItems(items)
	m.repoList.SetFilterText(filter)
	m.repoList.SetFilterState(list.Filtering)

	if selectedDir == "" {
		return
	}
	for i, it := range m.repoList.VisibleItems() {
		if ri, ok := it.(repoItem); ok && ri.projectDir == selectedDir {
			m.repoList.Select(i)
			return
		}
	}
}

// selectRepo creates a session for the selected project directory.
func (m *Model) selectRepo(item repoItem, withWorkspace bool) tea.Cmd {
	m.mode = viewDashboard
	m.statusMsg = "セッション作成中..."

	mgr := m.manager
	ctx := m.ctx
	return func() tea.Msg {
		sess, err := mgr.CreateSession(ctx, item.repoPath, item.projectDir, withWorkspace)
		var id session.DeckSessionID
		if sess != nil {
			id = sess.ID
		}
		return sessionCreatedMsg{sessionID: id, err: err}
	}
}

// withoutNestedRepos drops the project dirs under root that belong to another jj
// repository nested inside it (the nested root itself included).
//
// WHY: 入れ子のリポジトリの中身は外側のリポジトリが管理していない。外側のサブプロジェクトとして
// 選ぶと repo_path が外側になり、外側に作ったワークスペースにはそのディレクトリが無いので、
// ワークスペース付きの起動や fork が失敗する。入れ子のリポジトリは自分の候補として別に一覧に出る。
func withoutNestedRepos(root string, dirs, repoRoots []string) []string {
	var nested []string
	for _, r := range repoRoots {
		if r != root && isWithin(r, root) {
			nested = append(nested, r)
		}
	}
	var out []string
	for _, dir := range dirs {
		inNested := false
		for _, n := range nested {
			if isWithin(dir, n) {
				inNested = true
				break
			}
		}
		if !inNested {
			out = append(out, dir)
		}
	}
	return out
}

// isWithin reports whether path is base or a directory under it.
func isWithin(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
