package jj

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Command is the jj executable path. Override from config before use.
var Command = "jj"

// CreateWorkspaceAt creates a new jj workspace at the specified path.
// The parent directory is created automatically if it doesn't exist.
// colocated リポジトリの場合、ワークスペースに .git の symlink を作成して
// gh 等の git 依存ツールが動作するようにする。
// extraSymlinks にはリポジトリルートからの相対パスを指定し、ワークスペースに symlink を作成する。
func CreateWorkspaceAt(repoPath, name, wsPath string, extraSymlinks []string) error {
	if err := os.MkdirAll(filepath.Dir(wsPath), 0o755); err != nil {
		return fmt.Errorf("creating workspace parent dir: %w", err)
	}

	cmd := exec.Command(Command, "workspace", "add", "--name", name, wsPath)
	cmd.Dir = repoPath

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("jj workspace add: %s: %w", strings.TrimSpace(string(output)), err)
	}

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

// ForgetWorkspace removes a jj workspace.
func ForgetWorkspace(repoPath, name string) error {
	cmd := exec.Command(Command, "workspace", "forget", name)
	cmd.Dir = repoPath

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("jj workspace forget: %s: %w", strings.TrimSpace(string(output)), err)
	}

	return nil
}

