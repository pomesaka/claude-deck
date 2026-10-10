package session

import (
	"time"

	"github.com/pomesaka/claude-deck/internal/store"
)

// StatusFromID parses the value returned by Status.ID.
func StatusFromID(id string) (Status, bool) {
	for _, s := range []Status{
		StatusRunning, StatusWaitingApproval, StatusWaitingAnswer,
		StatusCompleted, StatusError, StatusIdle, StatusUnmanaged, StatusSubagentRunning,
	} {
		if s.ID() == id {
			return s, true
		}
	}
	return 0, false
}

// recordLocked converts the session to a store row. Caller must hold s.mu (read).
func (s *Session) recordLocked() store.Record {
	chain := make([]string, len(s.SessionChain))
	for i, id := range s.SessionChain {
		chain[i] = string(id)
	}
	return store.Record{
		ID:                   string(s.ID),
		Name:                 s.Name,
		Alias:                s.Alias,
		RepoPath:             s.RepoPath,
		RepoName:             s.RepoName,
		WorkspacePath:        s.WorkspacePath,
		WorkspaceName:        s.WorkspaceName,
		SubProjectDir:        s.SubProjectDir,
		SessionChain:         chain,
		ForkedFrom:           string(s.ForkedFrom),
		Status:               s.Status.ID(),
		FinishedAt:           copyTimePtr(s.FinishedAt),
		PID:                  s.PID,
		ErrorMessage:         s.ErrorMessage,
		TerminalTitle:        s.TerminalTitle,
		BookmarkName:         s.BookmarkName,
		LastJJRevision:       s.LastJJRevision,
		LastJJParentRevision: s.LastJJParentRevision,
		Prompt:               s.Prompt,
		PermissionMode:       s.PermissionMode,
		StartedAt:            s.StartedAt,
		LastActivity:         s.LastActivity,
	}
}

// newSessionFromRecord builds an in-memory session from a store row.
func newSessionFromRecord(r store.Record) *Session {
	s := &Session{
		ID:            DeckSessionID(r.ID),
		RepoPath:      r.RepoPath,
		RepoName:      r.RepoName,
		SubProjectDir: r.SubProjectDir,
		ForkedFrom:    RuntimeSessionID(r.ForkedFrom),
	}
	s.applyControlRecordLocked(r)
	s.applyProjectionRecordLocked(r)
	return s
}

// applyControlRecordLocked copies the fields that any process may write
// (CLI, hook commands, the pane's exit command) from the store row.
// Caller must hold s.mu (write) or own s exclusively.
func (s *Session) applyControlRecordLocked(r store.Record) {
	s.Name = r.Name
	s.Alias = r.Alias
	s.WorkspacePath = r.WorkspacePath
	s.WorkspaceName = r.WorkspaceName
	s.SessionChain = s.SessionChain[:0]
	for _, id := range r.SessionChain {
		s.SessionChain = append(s.SessionChain, ClaudeSessionID(id))
	}
	if len(s.SessionChain) == 0 {
		s.SessionChain = nil
	}
	if status, ok := StatusFromID(r.Status); ok {
		s.Status = status
	}
	s.FinishedAt = copyTimePtr(r.FinishedAt)
	s.PID = r.PID
	s.ErrorMessage = r.ErrorMessage
	s.LastJJRevision = r.LastJJRevision
	s.LastJJParentRevision = r.LastJJParentRevision
}

// applyProjectionRecordLocked copies the fields the TUI projects from JSONL
// and jj. Only applied when a session first appears in memory: after that the
// TUI's in-memory values are newer than what it last wrote to the store.
func (s *Session) applyProjectionRecordLocked(r store.Record) {
	s.TerminalTitle = r.TerminalTitle
	s.BookmarkName = r.BookmarkName
	s.Prompt = r.Prompt
	s.PermissionMode = r.PermissionMode
	s.StartedAt = r.StartedAt
	s.LastActivity = r.LastActivity
}

// copyProjection copies the TUI-projected fields of src into r.
// An empty BookmarkName is not copied: the process that created the session
// writes it after the TUI may have loaded the row, and the TUI refreshes it only
// for running sessions, so an empty in-memory value means "not read", not "none".
func copyProjection(r *store.Record, src store.Record) {
	r.TerminalTitle = src.TerminalTitle
	if src.BookmarkName != "" {
		r.BookmarkName = src.BookmarkName
	}
	r.Prompt = src.Prompt
	r.PermissionMode = src.PermissionMode
	r.StartedAt = src.StartedAt
	r.LastActivity = src.LastActivity
}

func copyTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
