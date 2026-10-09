package control

import (
	"context"
	"fmt"

	"github.com/pomesaka/claude-deck/internal/session"
)

// ManagerHandler executes control requests against the TUI's session.Manager,
// taking the same paths as the n / x keys.
type ManagerHandler struct {
	ctx context.Context
	mgr *session.Manager
}

// NewManagerHandler returns a Handler backed by mgr.
func NewManagerHandler(ctx context.Context, mgr *session.Manager) *ManagerHandler {
	return &ManagerHandler{ctx: ctx, mgr: mgr}
}

// New creates a session in dir, like pressing n and choosing a repository.
func (h *ManagerHandler) New(dir string, withWorkspace bool) (SessionInfo, error) {
	repoPath, workingDir, isJJ := session.ResolveLaunchDir(dir)
	if withWorkspace && !isJJ {
		return SessionInfo{}, fmt.Errorf("%s は jj リポジトリではないためワークスペースを作れません（--no-workspace で直接起動できます）", dir)
	}
	sess, err := h.mgr.Launch(h.ctx, session.LaunchIntent{
		Kind:          session.LaunchNew,
		RepoPath:      repoPath,
		WorkingDir:    workingDir,
		WithWorkspace: withWorkspace,
	})
	if err != nil {
		return SessionInfo{}, err
	}
	return infoFromSnapshot(sess.Snapshot()), nil
}

// List returns all sessions in the same order as the TUI list.
func (h *ManagerHandler) List() []SessionInfo {
	sessions := h.mgr.ListSessions()
	infos := make([]SessionInfo, len(sessions))
	for i, s := range sessions {
		infos[i] = infoFromSnapshot(s.Snapshot())
	}
	return infos
}

// Close stops the session and removes its workspace, like pressing x.
// It can be resumed later with r.
func (h *ManagerHandler) Close(target string) (SessionInfo, error) {
	sess, err := h.mgr.FindSession(target)
	if err != nil {
		return SessionInfo{}, err
	}
	if err := h.mgr.Kill(sess.ID); err != nil {
		return SessionInfo{}, err
	}
	return infoFromSnapshot(sess.Snapshot()), nil
}

func infoFromSnapshot(s session.Snapshot) SessionInfo {
	return SessionInfo{
		ID:              string(s.ID),
		Name:            s.Name,
		RepoPath:        s.RepoPath,
		WorkDir:         s.WorkDir(),
		Status:          s.Status.ID(),
		ClaudeSessionID: string(s.ClaudeSessionID),
	}
}
