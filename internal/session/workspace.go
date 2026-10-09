package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/jj"
	"github.com/pomesaka/claude-deck/internal/store"
)

// workspaces creates and removes the jj workspaces of deck sessions, which live
// under dataDir/workspace/<encoded repo>/<name>. It keeps no session state:
// callers pass the store rows a decision depends on.
type workspaces struct {
	dataDir string
	jj      *jj.Runner
	// symlinks returns the untracked paths to link into a new workspace. Nil links nothing.
	symlinks func(repoPath string) []string
	// trust and forget are ManagerConfig.TrustWorkspaceFunc and ForgetProjectsFunc.
	trust  func(wsPath string) error
	forget func(match func(dir string) bool, dryRun bool) (int, error)
}

func newWorkspaces(cfg ManagerConfig) workspaces {
	return workspaces{
		dataDir:  cfg.DataDir,
		jj:       cfg.jjRunner(),
		symlinks: cfg.WorkspaceSymlinksFunc,
		trust:    cfg.TrustWorkspaceFunc,
		forget:   cfg.ForgetProjectsFunc,
	}
}

// CollectGarbage removes what no session in the store owns any more: workspace
// directories without a row, and the runtime's records about workspaces that
// are gone. It needs neither a Manager nor tmux.
func CollectGarbage(st *store.Store, cfg ManagerConfig, dryRun bool) (GCReport, error) {
	recs, err := st.List()
	if err != nil {
		return GCReport{}, err
	}
	return newWorkspaces(cfg).collectGarbage(recs, dryRun)
}

// remove removes the workspace of a session being closed and returns where its
// working copy was (@ and @-), for the resume to recreate it (ADR 009). Both
// are empty when they could not be read.
func (ws workspaces) remove(repoPath, wsName string) (atRev, parentRev string) {
	wsRoot := ws.root(repoPath, wsName)
	// WHY cleanup より前に読む: jj workspace forget の後は @ の change_id が取れないことがある。
	// @ が空だと forget で abandon されるので、@- も控えておく。
	// 注意: 呼び出し元の StopProcess は SIGTERM を送るだけで終了を待たない。ここの jj log は
	// Claude Code の jj 操作と競合しうるが、cleanup の jj workspace forget も同じ前提で動いている。
	atRev, parentRev, err := ws.jj.GetWorkspaceRevisions(wsRoot)
	if err != nil {
		debuglog.Printf("[workspaces.remove] GetWorkspaceRevisions failed: %v", err)
		atRev, parentRev = "", ""
	}
	if w := ws.cleanup(repoPath, wsName, wsRoot); w != "" {
		debuglog.Printf("[workspaces.remove] workspace cleanup: %s", w)
	}
	return atRev, parentRev
}

// cleanup runs jj workspace forget and removes the workspace directory.
// wsRootPath is the workspace root directory to delete (DataDir/workspace/<encoded>/<name>).
// It may differ from sess.WorkspacePath, which can point to a subproject subdirectory.
// Returns a warning string if any operation failed, or "" on success/skip.
func (ws workspaces) cleanup(repoPath, wsName, wsRootPath string) string {
	if wsName == "" || repoPath == "" {
		return ""
	}
	var warnings []string
	// jj ワークスペースを forget してディレクトリを削除する。
	// close でも呼ばれる（resume では recreate が作り直す）。
	if err := ws.jj.ForgetWorkspace(repoPath, wsName); err != nil {
		// forget 失敗でもディレクトリ削除は続行する（jj が既に forget 済みの場合など）
		warnings = append(warnings, fmt.Sprintf("workspace forget失敗: %v", err))
	}
	if wsRootPath != "" {
		// 安全ガード: DataDir/workspace/ 配下のパスのみ削除する。
		// symlink (macOS: /var → /private/var) を解決してからプレフィックスを比較する。
		resolved := wsRootPath
		if r, err := filepath.EvalSymlinks(wsRootPath); err == nil {
			resolved = r
		}
		base := filepath.Join(ws.dataDir, "workspace") + string(filepath.Separator)
		if resolvedBase, err := filepath.EvalSymlinks(filepath.Join(ws.dataDir, "workspace")); err == nil {
			base = resolvedBase + string(filepath.Separator)
		}
		if strings.HasPrefix(resolved, base) {
			if err := os.RemoveAll(wsRootPath); err != nil {
				warnings = append(warnings, fmt.Sprintf("workspace ディレクトリ削除失敗: %v", err))
			}
		}
	}
	return strings.Join(warnings, "; ")
}

// root returns where the jj workspace named wsName of repoPath lives.
func (ws workspaces) root(repoPath, wsName string) string {
	return filepath.Join(ws.dataDir, "workspace", encodePathForDir(repoPath), wsName)
}

// create creates the jj workspace wsName of repoPath with the project's
// extra symlinks, marks it trusted for the runtime, and returns its root.
// opts.ExtraSymlinks is filled in here.
func (ws workspaces) create(repoPath, wsName string, opts jj.WorkspaceOptions) (string, error) {
	wsPath := ws.root(repoPath, wsName)
	if ws.symlinks != nil {
		opts.ExtraSymlinks = ws.symlinks(repoPath)
	}
	debuglog.Printf("[createWorkspace] name=%q path=%q", wsName, wsPath)
	if err := ws.jj.CreateWorkspaceAt(repoPath, wsName, wsPath, opts); err != nil {
		return "", err
	}
	if ws.trust != nil {
		// 失敗しても起動は続ける。trust ダイアログが出るだけで、利用者がその場で承認できる。
		if err := ws.trust(wsPath); err != nil {
			debuglog.Printf("[createWorkspace] trusting %q failed: %v", wsPath, err)
		}
	}
	return wsPath, nil
}

// discard removes a workspace created for a session that failed to start.
func (ws workspaces) discard(repoPath, wsName string) {
	wsRootPath := ws.root(repoPath, wsName)
	if w := ws.cleanup(repoPath, wsName, wsRootPath); w != "" {
		debuglog.Printf("[discardWorkspace] %s", w)
	}
}

// recreate creates a new jj workspace for a session whose workspace was deleted.
// atRev/parentRev は Kill 時に保存した @ / @- の change_id（ADR 009）。
// Returns the effective work directory (wsPath/subProjectDir if subProjectDir is set).
func (ws workspaces) recreate(repoPath, sessName, subProjectDir, atRev, parentRev string) (string, error) {
	debuglog.Printf("[recreateWorkspace] repoPath=%q sessName=%q atRev=%q parentRev=%q", repoPath, sessName, atRev, parentRev)
	wsPath, err := ws.create(repoPath, sessName, jj.WorkspaceOptions{AtRev: atRev, ParentRev: parentRev})
	if err != nil {
		return "", fmt.Errorf("recreating jj workspace: %w", err)
	}
	if subProjectDir != "" {
		return filepath.Join(wsPath, subProjectDir), nil
	}
	return wsPath, nil
}

// discardPruned removes what a pruned session left behind: its jj workspace, if
// it still has one, and the runtime's records about the workspace directory.
// A pruned session is gone from the list and cannot be resumed, so nothing
// would ever remove them otherwise.
func (ws workspaces) discardPruned(r store.Record, kept []store.Record) {
	if r.RepoPath == "" || r.Name == "" {
		return
	}
	// ワークスペースの名前はセッション名と同じ（CreateSession / ResumeSession）。
	// 同じ名前の行が残っているなら、そのワークスペースはまだ使われうるので触らない
	// （JSON の store 時代の重複行が該当する）。
	for _, k := range kept {
		if k.RepoPath == r.RepoPath && k.Name == r.Name {
			debuglog.Printf("[discardPruned] %s: workspace %q still belongs to %s", r.ID, r.Name, k.ID)
			return
		}
	}
	wsRoot := ws.root(r.RepoPath, r.Name)

	// WorkspaceName が空の行は、close でワークスペースを消してある。
	if r.WorkspaceName != "" {
		if _, err := os.Stat(wsRoot); err == nil {
			// 編集途中のファイルを snapshot で @ に取り込んでから forget する（GetNearestBookmark の WHY NOT 参照）。
			// WHY 失敗したら消さない: snapshot できていない変更は、ディレクトリを消すと取り戻せない。
			// 行はもう無いので、消さなかったワークスペースは手で片付けることになる。
			atRev, parentRev, err := ws.jj.GetWorkspaceRevisions(wsRoot)
			if err != nil {
				debuglog.Printf("[discardPruned] %s: keeping workspace %s, snapshot failed: %v", r.ID, wsRoot, err)
				return
			}
			debuglog.Printf("[discardPruned] %s: removing workspace %s (@=%s @-=%s)", r.ID, wsRoot, atRev, parentRev)
		}
		if w := ws.cleanup(r.RepoPath, r.Name, wsRoot); w != "" {
			debuglog.Printf("[discardPruned] %s: workspace cleanup: %s", r.ID, w)
		}
	}

	if ws.forget != nil {
		below := wsRoot + string(filepath.Separator)
		match := func(dir string) bool { return dir == wsRoot || strings.HasPrefix(dir, below) }
		if _, err := ws.forget(match, false); err != nil {
			debuglog.Printf("[discardPruned] %s: forgetting %s: %v", r.ID, wsRoot, err)
		}
	}
}

// gcMinAge is how long a workspace directory must have existed before
// CollectGarbage may remove it.
// WHY: CreateSession はワークスペースを作ってから store に行を入れる。その間に別プロセスの gc が
// 動くと、行がまだ無いワークスペースを持ち主なしと判定してしまう。
const gcMinAge = time.Hour

// GCReport is what CollectGarbage removed, or with DryRun would remove.
type GCReport struct {
	DryRun bool
	// Workspaces are the workspace directories no store row owns.
	Workspaces []GCWorkspace
	// ForgottenProjects counts the runtime's records about workspaces that are gone.
	ForgottenProjects int
}

// GCWorkspace is one workspace directory CollectGarbage removed.
type GCWorkspace struct {
	Path string
	// Warning is set when the removal was incomplete (jj forget or the delete failed).
	Warning string
}

// collectGarbage removes what none of the rows recs owns any more: workspace directories
// without a store row, and the runtime's records about workspaces that are gone.
// Prune does this for the sessions it deletes; this catches what is left when a
// prune was interrupted or kept a workspace it could not snapshot.
//
// A workspace is owned when a row has its repository and name, finished or not:
// a closed session is resumed into the same directory.
func (ws workspaces) collectGarbage(recs []store.Record, dryRun bool) (GCReport, error) {
	owned := make(map[string]bool, len(recs))
	for _, r := range recs {
		if r.RepoPath != "" && r.Name != "" {
			owned[ws.root(r.RepoPath, r.Name)] = true
		}
	}

	base := filepath.Join(ws.dataDir, "workspace")
	report := GCReport{DryRun: dryRun}
	removing := make(map[string]bool)
	now := time.Now()
	repoDirs, err := os.ReadDir(base)
	if err != nil && !os.IsNotExist(err) {
		return GCReport{}, err
	}
	for _, repoDir := range repoDirs {
		if !repoDir.IsDir() {
			continue
		}
		wsDirs, err := os.ReadDir(filepath.Join(base, repoDir.Name()))
		if err != nil {
			return GCReport{}, err
		}
		for _, wsDir := range wsDirs {
			wsRoot := filepath.Join(base, repoDir.Name(), wsDir.Name())
			if !wsDir.IsDir() || owned[wsRoot] {
				continue
			}
			if info, err := wsDir.Info(); err != nil || now.Sub(info.ModTime()) < gcMinAge {
				continue
			}
			found := GCWorkspace{Path: wsRoot}
			if !dryRun {
				found.Warning = ws.removeOrphan(wsRoot, wsDir.Name())
			}
			removing[wsRoot] = true
			report.Workspaces = append(report.Workspaces, found)
		}
	}

	if ws.forget != nil {
		// 記録のキーは、ワークスペースのルートか、その配下のディレクトリ（サブプロジェクト）。
		match := func(dir string) bool {
			rel, ok := strings.CutPrefix(dir, base+string(filepath.Separator))
			if !ok {
				return false
			}
			parts := strings.SplitN(rel, string(filepath.Separator), 3)
			if len(parts) < 2 {
				return false
			}
			wsRoot := filepath.Join(base, parts[0], parts[1])
			if owned[wsRoot] {
				return false
			}
			if removing[wsRoot] {
				return true
			}
			_, err := os.Stat(wsRoot)
			return os.IsNotExist(err)
		}
		n, err := ws.forget(match, dryRun)
		if err != nil {
			return report, err
		}
		report.ForgottenProjects = n
	}
	return report, nil
}

// removeOrphan removes a workspace directory that no store row owns.
// Returns a warning when the removal was incomplete.
func (ws workspaces) removeOrphan(wsRoot, wsName string) string {
	if _, err := os.Stat(filepath.Join(wsRoot, ".jj")); err != nil {
		// jj のワークスペースになっていないディレクトリ（作成の途中で止まったものなど）。
		// ここで jj を呼ぶと、jj は親ディレクトリを辿って別のリポジトリを操作しうる。
		if err := os.RemoveAll(wsRoot); err != nil {
			return fmt.Sprintf("workspace ディレクトリ削除失敗: %v", err)
		}
		return ""
	}
	// WHY snapshot に失敗しても消す（discardPruned は消さない）: gc は、snapshot できずに残った
	// ワークスペースを片付ける手段でもある。ここで止めると、作業コピーが stale なワークスペースを
	// 消す方法が無くなる。
	if atRev, parentRev, err := ws.jj.GetWorkspaceRevisions(wsRoot); err != nil {
		debuglog.Printf("[removeOrphanWorkspace] %s: snapshot failed, removing anyway: %v", wsRoot, err)
	} else {
		debuglog.Printf("[removeOrphanWorkspace] %s: @=%s @-=%s", wsRoot, atRev, parentRev)
	}
	// 行が無いのでリポジトリのパスは分からない。jj はワークスペースの中からでも forget できる。
	return ws.cleanup(wsRoot, wsName, wsRoot)
}
