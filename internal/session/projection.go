package session

import (
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
)

// Session state is projected (assembled) from several sources:
//
//	Store  → ID, Name, RepoPath, RepoName, WorkspacePath, WorkspaceName,
//	         SubProjectDir, SessionChain, Status, FinishedAt, PID
//	JSONL  → Prompt, PermissionMode, StartedAt, LastActivity
//	jj     → BookmarkName
//
// The Apply* methods below are the only places the JSONL and jj fields are
// written; the store fields are written by applyControlRecordLocked.

// ApplyFileActivity updates LastActivity from a JSONL file write event.
// This is the single point where file modification timestamps flow into Session state.
func (s *Session) ApplyFileActivity(modTime time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastActivity = modTime
}

// ApplyBookmark updates the nearest jj bookmark name.
// Only updates if the new bookmark differs from the current one.
func (s *Session) ApplyBookmark(bookmark string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.BookmarkName != bookmark {
		debuglog.Printf("[session:%s] bookmark %s -> %s", s.ID, s.BookmarkName, bookmark)
		s.BookmarkName = bookmark
	}
}
