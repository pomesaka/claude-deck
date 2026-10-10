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

// WorkspaceOptions configures revision-restoration behavior for CreateWorkspaceAt.
// AtRev and ParentRev are empty when creating a new session (trunk() is used).
// On resume they carry the change_ids captured at Kill time (ADR 009).
type WorkspaceOptions struct {
	ExtraSymlinks []string // repo-relative paths to symlink into the workspace
	AtRev         string   // change_id of @ at kill time; jj edit attempts direct restore
	ParentRev     string   // change_id of @- at kill time; fallback when AtRev is abandoned
}

func (r *Runner) command() string {
	if r.Command != "" {
		return r.Command
	}
	return "jj"
}

// CreateWorkspaceAt creates a new jj workspace at the specified path.
// The parent directory is created automatically if it doesn't exist.
// For colocated repos a .git symlink is created so git-dependent tools work.
//
// Revision restore order on resume (ADR 009):
//  1. opts.AtRev non-empty: try jj edit <AtRev>. Restores the exact commit
//     including any in-progress changes, without creating an extra commit.
//     jj workspace add creates an initial empty @; jj edit abandons it (expected).
//     Falls through on failure (AtRev was abandoned by workspace forget).
//  2. opts.ParentRev non-empty: try jj new <ParentRev> (last committed state).
//     Falls through on failure.
//  3. jj new trunk() as final fallback.
//
// For new/fork sessions pass zero-value WorkspaceOptions (trunk() is used).
func (r *Runner) CreateWorkspaceAt(repoPath, name, wsPath string, opts WorkspaceOptions) error {
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

	if err := r.setUpWorkspace(repoPath, wsPath, opts); err != nil {
		// WHY 作りかけを消す: 残すと、同じ名前での作り直し（resume）が失敗し続ける。jj workspace add は
		// 名前が登録済みなら "Workspace named ... already exists"、ディレクトリだけ残っていても
		// "Destination path exists and is not an empty directory" で失敗する（jj 0.37.0 で確認）。
		// WHY NOT add の失敗でも消す: そのときの名前とディレクトリは、この呼び出しが作ったものではない。
		if ferr := r.ForgetWorkspace(repoPath, name); ferr != nil {
			debuglog.Printf("[jj.CreateWorkspaceAt] undoing the add: %v", ferr)
		}
		if rerr := os.RemoveAll(wsPath); rerr != nil {
			debuglog.Printf("[jj.CreateWorkspaceAt] undoing the add: %v", rerr)
		}
		return err
	}
	return nil
}

// setUpWorkspace prepares the workspace jj workspace add has just created at
// wsPath: the symlinks, and the revision to start from.
func (r *Runner) setUpWorkspace(repoPath, wsPath string, opts WorkspaceOptions) error {
	jjCmd := r.command()

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
	for _, rel := range opts.ExtraSymlinks {
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

	// revision 復元: AtRev → ParentRev → trunk() の順で試みる（ADR 009）。
	// @ の change_id が保存されている場合は jj edit で直接そのコミットに戻す。
	// jj new と異なり余分なコミットが増えず、変更中だったファイルもそのまま復元できる。
	if opts.AtRev != "" {
		editCmd := exec.Command(jjCmd, "edit", opts.AtRev)
		editCmd.Dir = wsPath
		if out, err := editCmd.CombinedOutput(); err != nil {
			// @ が abandon 済みの場合（空のまま workspace forget された）は次の fallback へ。
			debuglog.Printf("[jj.CreateWorkspaceAt] jj edit %s failed: %v output=%q, falling back to opts.ParentRev", opts.AtRev, err, strings.TrimSpace(string(out)))
		} else {
			debuglog.Printf("[jj.CreateWorkspaceAt] jj edit %s done", opts.AtRev)
			return nil
		}
	}

	// @- の change_id が保存されている場合は jj new <opts.ParentRev> でその上から再開。
	if opts.ParentRev != "" {
		newCmd := exec.Command(jjCmd, "new", opts.ParentRev)
		newCmd.Dir = wsPath
		if out, err := newCmd.CombinedOutput(); err != nil {
			debuglog.Printf("[jj.CreateWorkspaceAt] jj new %s failed: %v output=%q, falling back to trunk()", opts.ParentRev, err, strings.TrimSpace(string(out)))
		} else {
			debuglog.Printf("[jj.CreateWorkspaceAt] jj new %s done", opts.ParentRev)
			return nil
		}
	}

	// 最終 fallback: trunk() から新規 revision を作成。
	debuglog.Printf("[jj.CreateWorkspaceAt] running: jj new trunk()")
	trunkCmd := exec.Command(jjCmd, "new", "trunk()")
	trunkCmd.Dir = wsPath
	if out, err := trunkCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("jj new trunk(): %s: %w", strings.TrimSpace(string(out)), err)
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

// GetWorkspaceRevisions returns the change_ids of the working copy (@) and its parent (@-).
// Called before jj workspace forget to preserve the revision for later restore via ResumeSession.
// @ may be abandoned on workspace forget if it has no changes; @- is always preserved.
//
// Error handling is intentionally asymmetric: err is non-nil only when @ cannot be retrieved.
// Failure to retrieve @- (e.g., @ is root) is treated as success with an empty parentRev.
// Callers should treat both fields as valid only when err == nil.
func (r *Runner) GetWorkspaceRevisions(wsPath string) (atRev, parentRev string, err error) {
	jjCmd := r.command()

	atCmd := exec.Command(jjCmd, "log", "--no-graph", "--color=never", "-r", "@", "-T", "change_id")
	atCmd.Dir = wsPath
	atOut, atErr := atCmd.CombinedOutput()
	if atErr != nil {
		return "", "", fmt.Errorf("jj log @: %w: %s", atErr, strings.TrimSpace(string(atOut)))
	}

	parentCmd := exec.Command(jjCmd, "log", "--no-graph", "--color=never", "-r", "@-", "-T", "change_id")
	parentCmd.Dir = wsPath
	parentOut, parentErr := parentCmd.CombinedOutput() // @ が root の場合 @- は存在しないため無視
	if parentErr != nil {
		debuglog.Printf("[jj.GetWorkspaceRevisions] jj log @- failed (@ may be root): %v output=%q", parentErr, strings.TrimSpace(string(parentOut)))
	}

	return strings.TrimSpace(string(atOut)), strings.TrimSpace(string(parentOut)), nil
}

// GetNearestBookmark returns the local bookmark name of the closest ancestor
// (including @) that has a bookmark. Returns empty string if none found.
func (r *Runner) GetNearestBookmark(dir string) (string, error) {
	debuglog.Printf("[jj.GetNearestBookmark] dir=%q", dir)
	// WHY --ignore-working-copy: ブランチ表示のために繰り返し呼ばれる読み取り専用の問い合わせ。
	//   付けないと jj は毎回「作業コピーの snapshot」と「git の ref の取り込み」を操作として書き込み、
	//   ユーザーやエージェントの jj git fetch / jj new と重なると操作が分岐して、ローカルの bookmark
	//   （main 等）が意図しないコミットを指したまま統合される（jj 0.37.0・2026-09-30 に実際に 2 回発生）。
	//   付けると操作を 1 件も書かないことを使い捨てのリポジトリで確認した
	// WHY NOT GetWorkspaceRevisions にも付ける: Kill 直前の @ の記録は、編集途中のファイルを snapshot で
	//   @ に取り込んでから行う必要がある（付けると未 snapshot の変更が workspace forget で失われる）
	cmd := exec.Command(r.command(), "--ignore-working-copy", "log", "--no-graph", "--color=never",
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

// ForgetWorkspace removes a jj workspace. dir is any directory of the
// repository: its root, or one of its workspaces.
//
// WHY --ignore-working-copy: forget は dir の作業コピーを必要としない。付けないと dir の作業コピーを
// snapshot する操作が 1 件余分に書かれ（GetNearestBookmark の WHY と同じ分岐の原因になる）、
// 作業コピーが stale だと forget そのものが失敗する。付けると forget の操作 1 件だけになることを
// 使い捨てのリポジトリで確認した（jj 0.37.0）。
// 消すワークスペースの snapshot が要る呼び出し元は、先に GetWorkspaceRevisions を呼ぶ。
func (r *Runner) ForgetWorkspace(dir, name string) error {
	cmd := exec.Command(r.command(), "--ignore-working-copy", "workspace", "forget", name)
	cmd.Dir = dir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("jj workspace forget: %s: %w", strings.TrimSpace(string(output)), err)
	}

	return nil
}

