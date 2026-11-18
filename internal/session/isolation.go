package session

import "github.com/pomesaka/claude-deck/internal/store"

// Isolation tells where a deck session's process runs: directly in a
// directory of the repository, or in a jj workspace claude-deck made for it.
// A workspace session keeps its kind after close removes the workspace; the
// resume makes it again from the revisions saved here.
//
// The workspace is named after the session (Session.Name) and lives where
// workspaces.root puts it, so neither is held here.
//
// The zero value is a direct session with no directory.
type Isolation struct {
	workspace bool
	// workDir is the directory the process runs in: the directory itself for a
	// direct session, <workspace root>/<SubProjectDir> for a workspace. Empty
	// once the workspace is removed.
	workDir string
	removed bool
	// atRev and parentRev are the change IDs of @ and @- when close removed the
	// workspace. Empty when they could not be read.
	atRev, parentRev string
}

// DirectIsolation is a session started in dir without a workspace.
func DirectIsolation(dir string) Isolation { return Isolation{workDir: dir} }

// WorkspaceIsolation is a session started in workDir, inside its jj workspace.
func WorkspaceIsolation(workDir string) Isolation {
	return Isolation{workspace: true, workDir: workDir}
}

// Removed is the isolation after close removed the workspace, which was at
// atRev (@) and parentRev (@-). A direct session has nothing to remove.
func (i Isolation) Removed(atRev, parentRev string) Isolation {
	if !i.workspace {
		return i
	}
	return Isolation{workspace: true, removed: true, atRev: atRev, parentRev: parentRev}
}

// WorkDir is where the process runs, or "" while the workspace is removed.
func (i Isolation) WorkDir() string { return i.workDir }

// workDirOr is where the session's process runs, or repoPath while its
// workspace is removed (or a direct session has no directory recorded).
func (i Isolation) workDirOr(repoPath string) string {
	if i.workDir != "" {
		return i.workDir
	}
	return repoPath
}

// IsWorkspace reports whether the session runs in a workspace of its own,
// whether or not the workspace exists now.
func (i Isolation) IsWorkspace() bool { return i.workspace }

// HasWorkspace reports whether the session's workspace exists.
func (i Isolation) HasWorkspace() bool { return i.workspace && !i.removed }

// Revisions are where the removed workspace was, for the resume to make it again.
func (i Isolation) Revisions() (atRev, parentRev string) { return i.atRev, i.parentRev }

// isolationFromRecord reads the isolation out of a store row. Together with
// isolationToRecord it is the only place the workspace columns are read or
// written as isolation.
func isolationFromRecord(r store.Record) Isolation {
	kind := r.Isolation
	if kind == "" { // a row built in memory, not read from the store
		kind = store.LegacyIsolation(r.Name, r.WorkspaceName, r.WorkspacePath)
	}
	if kind != store.IsolationWorkspace {
		return DirectIsolation(r.WorkspacePath)
	}
	// workspace_name は「いまワークスペースがある」印。close が消すと空になる。
	if r.WorkspaceName == "" {
		return Isolation{workspace: true, removed: true, atRev: r.LastJJRevision, parentRev: r.LastJJParentRevision}
	}
	return WorkspaceIsolation(r.WorkspacePath)
}

// isolationToRecord writes i into the row's workspace columns. The workspace
// name column holds r.Name while the workspace exists, as older binaries read it.
func isolationToRecord(r *store.Record, i Isolation) {
	r.WorkspaceName, r.WorkspacePath = "", i.workDir
	r.LastJJRevision, r.LastJJParentRevision = i.atRev, i.parentRev
	r.Isolation = store.IsolationDirect
	if i.workspace {
		r.Isolation = store.IsolationWorkspace
		if !i.removed {
			r.WorkspaceName = r.Name
		}
	}
}
