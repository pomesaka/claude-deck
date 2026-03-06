package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pomesaka/claude-deck/internal/usage"
)

func TestResolveJJRepo_RealRepo(t *testing.T) {
	// .jj/repo がディレクトリ → 本体リポジトリ
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "myrepo")
	jjRepoDir := filepath.Join(repoDir, ".jj", "repo")
	if err := os.MkdirAll(jjRepoDir, 0o755); err != nil {
		t.Fatal(err)
	}

	info := resolveJJRepo(repoDir)
	if info == nil {
		t.Fatal("expected non-nil jjRepoInfo")
	}
	if info.JJParent != repoDir {
		t.Errorf("JJParent = %q, want %q", info.JJParent, repoDir)
	}
	if info.RepoRoot != repoDir {
		t.Errorf("RepoRoot = %q, want %q", info.RepoRoot, repoDir)
	}
	if info.IsWorkspace {
		t.Error("expected IsWorkspace = false")
	}
}

func TestResolveJJRepo_RealRepo_Subdir(t *testing.T) {
	// サブディレクトリから上方向に探索して .jj/repo を見つける
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "myrepo")
	jjRepoDir := filepath.Join(repoDir, ".jj", "repo")
	subDir := filepath.Join(repoDir, "src", "pkg")
	if err := os.MkdirAll(jjRepoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	info := resolveJJRepo(subDir)
	if info == nil {
		t.Fatal("expected non-nil jjRepoInfo")
	}
	if info.RepoRoot != repoDir {
		t.Errorf("RepoRoot = %q, want %q", info.RepoRoot, repoDir)
	}
	if info.IsWorkspace {
		t.Error("expected IsWorkspace = false")
	}
}

func TestResolveJJRepo_Workspace(t *testing.T) {
	// .jj/repo がファイル → ワークスペース
	tmp := t.TempDir()

	// 本体リポジトリ
	mainRepo := filepath.Join(tmp, "mainrepo")
	mainJJRepo := filepath.Join(mainRepo, ".jj", "repo")
	if err := os.MkdirAll(mainJJRepo, 0o755); err != nil {
		t.Fatal(err)
	}

	// ワークスペース: .jj/repo はファイルで中身は本体の .jj/repo パス
	wsDir := filepath.Join(tmp, "workspace", "anna-8cc7")
	wsJJDir := filepath.Join(wsDir, ".jj")
	if err := os.MkdirAll(wsJJDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsJJDir, "repo"), []byte(mainJJRepo), 0o644); err != nil {
		t.Fatal(err)
	}

	info := resolveJJRepo(wsDir)
	if info == nil {
		t.Fatal("expected non-nil jjRepoInfo")
	}
	if info.JJParent != wsDir {
		t.Errorf("JJParent = %q, want %q", info.JJParent, wsDir)
	}
	if info.RepoRoot != mainRepo {
		t.Errorf("RepoRoot = %q, want %q", info.RepoRoot, mainRepo)
	}
	if !info.IsWorkspace {
		t.Error("expected IsWorkspace = true")
	}
}

func TestResolveJJRepo_NoJJ(t *testing.T) {
	tmp := t.TempDir()
	info := resolveJJRepo(tmp)
	if info != nil {
		t.Errorf("expected nil, got %+v", info)
	}
}

func TestNewExternalSession_Workspace(t *testing.T) {
	tmp := t.TempDir()

	// 本体リポジトリ: ADeT-AI
	mainRepo := filepath.Join(tmp, "ADeT-AI")
	mainJJRepo := filepath.Join(mainRepo, ".jj", "repo")
	if err := os.MkdirAll(mainJJRepo, 0o755); err != nil {
		t.Fatal(err)
	}

	// ワークスペース: anna-8cc7
	wsDir := filepath.Join(tmp, "workspace", "anna-8cc7")
	wsJJDir := filepath.Join(wsDir, ".jj")
	if err := os.MkdirAll(wsJJDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsJJDir, "repo"), []byte(mainJJRepo), 0o644); err != nil {
		t.Fatal(err)
	}

	info := &usage.SessionInfo{
		SessionID: "abcd1234-5678-9012-3456-789012345678",
		CWD:       wsDir,
	}
	sess := newExternalSession(info)

	// Name はワークスペース名、RepoName は本体リポ名で異なること
	if sess.Name == sess.RepoName {
		t.Errorf("Name and RepoName should differ, both = %q", sess.Name)
	}
	if sess.Name != "anna-8cc7" {
		t.Errorf("Name = %q, want %q", sess.Name, "anna-8cc7")
	}
	if sess.RepoName != "ADeT-AI" {
		t.Errorf("RepoName = %q, want %q", sess.RepoName, "ADeT-AI")
	}
	if sess.RepoPath != mainRepo {
		t.Errorf("RepoPath = %q, want %q", sess.RepoPath, mainRepo)
	}
	if sess.WorkspacePath != wsDir {
		t.Errorf("WorkspacePath = %q, want %q", sess.WorkspacePath, wsDir)
	}
}

func TestNewExternalSession_MainRepo(t *testing.T) {
	tmp := t.TempDir()

	mainRepo := filepath.Join(tmp, "myproject")
	mainJJRepo := filepath.Join(mainRepo, ".jj", "repo")
	if err := os.MkdirAll(mainJJRepo, 0o755); err != nil {
		t.Fatal(err)
	}

	info := &usage.SessionInfo{
		SessionID: "abcd1234-5678-9012-3456-789012345678",
		CWD:       mainRepo,
	}
	sess := newExternalSession(info)

	if sess.Name != "abcd1234" {
		t.Errorf("Name = %q, want %q", sess.Name, "abcd1234")
	}
	if sess.RepoName != "myproject" {
		t.Errorf("RepoName = %q, want %q", sess.RepoName, "myproject")
	}
	if sess.RepoPath != mainRepo {
		t.Errorf("RepoPath = %q, want %q", sess.RepoPath, mainRepo)
	}
}

func TestNewExternalSession_MainRepoSubdir(t *testing.T) {
	tmp := t.TempDir()

	mainRepo := filepath.Join(tmp, "myproject")
	mainJJRepo := filepath.Join(mainRepo, ".jj", "repo")
	subDir := filepath.Join(mainRepo, "packages", "api")
	if err := os.MkdirAll(mainJJRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	info := &usage.SessionInfo{
		SessionID: "abcd1234-5678-9012-3456-789012345678",
		CWD:       subDir,
	}
	sess := newExternalSession(info)

	if sess.RepoName != "myproject" {
		t.Errorf("RepoName = %q, want %q", sess.RepoName, "myproject")
	}
	if sess.SubProjectDir != "packages/api" {
		t.Errorf("SubProjectDir = %q, want %q", sess.SubProjectDir, "packages/api")
	}
}

func TestNewExternalSession_NoJJ(t *testing.T) {
	tmp := t.TempDir()

	dir := filepath.Join(tmp, "somedir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	info := &usage.SessionInfo{
		SessionID: "abcd1234-5678-9012-3456-789012345678",
		CWD:       dir,
	}
	sess := newExternalSession(info)

	if sess.Name != "abcd1234" {
		t.Errorf("Name = %q, want %q", sess.Name, "abcd1234")
	}
	if sess.RepoName != "somedir" {
		t.Errorf("RepoName = %q, want %q", sess.RepoName, "somedir")
	}
	if sess.RepoPath != dir {
		t.Errorf("RepoPath = %q, want %q", sess.RepoPath, dir)
	}
}
