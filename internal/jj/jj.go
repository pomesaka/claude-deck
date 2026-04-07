package jj

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pomesaka/claude-deck/internal/debuglog"
)

// Runner executes jj CLI commands. Inject via ManagerConfig instead of
// relying on a package-level global, so tests can substitute a stub.
type Runner struct {
	Command string // jj executable path; defaults to "jj" if empty
}

func (r *Runner) command() string {
	if r.Command != "" {
		return r.Command
	}
	return "jj"
}

// CreateWorkspaceAt creates a new jj workspace at the specified path.
// The parent directory is created automatically if it doesn't exist.
// colocated リポジトリの場合、ワークスペースに .git の symlink を作成して
// gh 等の git 依存ツールが動作するようにする。
// extraSymlinks にはリポジトリルートからの相対パスを指定し、ワークスペースに symlink を作成する。
func (r *Runner) CreateWorkspaceAt(repoPath, name, wsPath string, extraSymlinks []string) error {
	debuglog.Printf("[jj.CreateWorkspaceAt] repoPath=%q name=%q wsPath=%q", repoPath, name, wsPath)
	if err := os.MkdirAll(filepath.Dir(wsPath), 0o755); err != nil {
		return fmt.Errorf("creating workspace parent dir: %w", err)
	}

	debuglog.Printf("[jj.CreateWorkspaceAt] running: jj workspace add --name %s %s", name, wsPath)
	jjCmd := r.command()
	cmd := exec.Command(jjCmd, "workspace", "add", "--name", name, wsPath)
	cmd.Dir = repoPath

	output, err := cmd.CombinedOutput()
	if err != nil {
		debuglog.Printf("[jj.CreateWorkspaceAt] jj workspace add failed: %v output=%q", err, string(output))
		return fmt.Errorf("jj workspace add: %s: %w", strings.TrimSpace(string(output)), err)
	}
	debuglog.Printf("[jj.CreateWorkspaceAt] jj workspace add done")

	// colocated リポジトリなら .git への symlink を作成
	gitDir := filepath.Join(repoPath, ".git")
	if info, err := os.Stat(gitDir); err == nil && info.IsDir() {
		link := filepath.Join(wsPath, ".git")
		if _, err := os.Lstat(link); os.IsNotExist(err) {
			if err := os.Symlink(gitDir, link); err != nil {
				return fmt.Errorf("symlinking .git to workspace: %w", err)
			}
		}
	}

	// プロジェクト設定で指定された追加 symlink を作成
	for _, rel := range extraSymlinks {
		if err := createExtraSymlink(repoPath, wsPath, rel); err != nil {
			return err
		}
	}

	// リモートから最新を取得し、trunk 上に新 revision を作成
	// fetch 失敗はネットワーク不通等で起こりうるので無視して続行
	debuglog.Printf("[jj.CreateWorkspaceAt] running: jj git fetch (may hang on network)")
	fetch := exec.Command(jjCmd, "git", "fetch")
	fetch.Dir = wsPath
	fetchOut, fetchErr := fetch.CombinedOutput()
	debuglog.Printf("[jj.CreateWorkspaceAt] jj git fetch done: err=%v output=%q", fetchErr, strings.TrimSpace(string(fetchOut)))

	debuglog.Printf("[jj.CreateWorkspaceAt] running: jj new trunk()")
	newCmd := exec.Command(jjCmd, "new", "trunk()")
	newCmd.Dir = wsPath
	if output, err := newCmd.CombinedOutput(); err != nil {
		debuglog.Printf("[jj.CreateWorkspaceAt] jj new trunk() failed: %v output=%q", err, string(output))
		return fmt.Errorf("jj new trunk(): %s: %w", strings.TrimSpace(string(output)), err)
	}
	debuglog.Printf("[jj.CreateWorkspaceAt] jj new trunk() done")

	return nil
}

// createExtraSymlink はリポジトリルートからの相対パスで指定されたファイル/ディレクトリの
// symlink をワークスペースに作成する。
// セキュリティ: 絶対パスや ".." を含むパスはスキップする。
func createExtraSymlink(repoPath, wsPath, rel string) error {
	// 絶対パスや親ディレクトリ参照を拒否
	if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return nil
	}

	src := filepath.Join(repoPath, rel)
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil // ソースが存在しなければスキップ
	}

	dst := filepath.Join(wsPath, rel)
	if _, err := os.Lstat(dst); err == nil {
		return nil // 宛先が既に存在すればスキップ
	}

	// ネストパスなら親ディレクトリを作成
	if dir := filepath.Dir(dst); dir != wsPath {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating parent dir for symlink %s: %w", rel, err)
		}
	}

	if err := os.Symlink(src, dst); err != nil {
		return fmt.Errorf("symlinking %s to workspace: %w", rel, err)
	}
	return nil
}

// GetNearestBookmark returns the local bookmark name of the closest ancestor
// (including @) that has a bookmark. Returns empty string if none found.
func (r *Runner) GetNearestBookmark(dir string) (string, error) {
	debuglog.Printf("[jj.GetNearestBookmark] dir=%q", dir)
	cmd := exec.Command(r.command(), "log", "--no-graph", "--color=never",
		"-r", "latest(::@ & bookmarks())",
		"-T", "bookmarks")
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		debuglog.Printf("[jj.GetNearestBookmark] failed: %v output=%q", err, strings.TrimSpace(string(output)))
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}

	raw := strings.TrimSpace(string(output))
	debuglog.Printf("[jj.GetNearestBookmark] raw=%q", raw)
	if raw == "" {
		return "", nil
	}

	// bookmarks テンプレートはスペース区切りで出力される。
	// リモート追跡ブックマークは "@origin" サフィックスが付く。
	// ローカルブックマーク（@ なし）のうち最初のものを返す。
	for _, name := range strings.Fields(raw) {
		if !strings.Contains(name, "@") {
			return name, nil
		}
	}
	return "", nil
}

// ForgetWorkspace removes a jj workspace.
func (r *Runner) ForgetWorkspace(repoPath, name string) error {
	cmd := exec.Command(r.command(), "workspace", "forget", name)
	cmd.Dir = repoPath

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("jj workspace forget: %s: %w", strings.TrimSpace(string(output)), err)
	}

	return nil
}

