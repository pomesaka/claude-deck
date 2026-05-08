package session

import (
	"context"
)

// ReconcileResult is returned by SessionBackend.Reconcile.
// It describes which sessions have live backend processes after reconciliation.
type ReconcileResult struct {
	// LivePIDs maps DeckSessionID → process PID for sessions whose backend
	// process is still running. Manager uses this to update Session.PID and
	// re-mark sessions as managed after a restart.
	LivePIDs map[DeckSessionID]int
}

// SessionBackend abstracts the process management layer for Claude Code sessions.
// It decouples Manager from the concrete mechanism used to host processes,
// enabling backend swapping without touching session domain logic.
//
// Responsibilities:
//   - Process lifecycle: start, stop, liveness check
//
// Non-responsibilities (stay in Manager/Session):
//   - Session object creation and persistence
//   - jj workspace management
//   - JSONL streaming and status tracking
type SessionBackend interface {
	// StartProcess launches a Claude Code process for the session.
	// The backend is responsible for:
	//   - calling sess.AttachProcess to wire the RunningProcess sentinel
	//   - spawning the exit-watcher goroutine that calls the onExit callback
	StartProcess(ctx context.Context, sess *Session, opts ProcessStartOpts, onOutput func([]byte)) error

	// StopProcess terminates the process for the given session.
	// fallbackPID is used when the process handle is unavailable (e.g., session
	// restored from store without a live handle).
	StopProcess(sessionID DeckSessionID, fallbackPID int) error

	// IsActive returns true if the session has a live, non-exited process.
	IsActive(sessionID DeckSessionID) bool

	// Focus makes the session's terminal visible in the hosting environment.
	// tmuxBackend: runs tmux select-window for ~0ms session switching.
	Focus(sessionID DeckSessionID) error

	// EnsurePreview creates the preview subprocess window if not already present.
	// The preview window displays JSONL logs for the selected session in split mode.
	EnsurePreview() error

	// FocusPreview switches the display to the preview window.
	// tmuxBackend: runs tmux select-window __preview__.
	FocusPreview() error

	// KillPreview destroys the preview window.
	// tmuxBackend: kills the __preview__ tmux window.
	KillPreview() error

	// Reconcile synchronises backend state against the provided session list.
	// For each session with a live backend process, the backend re-attaches its
	// exit watcher and includes the session ID in ReconcileResult.LivePIDs.
	// Orphaned backend processes (no corresponding deck session) are killed.
	Reconcile(sessions []*Session) (ReconcileResult, error)

	// Close releases all resources held by the backend.
	Close()
}

// ProcessStartOpts contains all parameters needed to start a Claude Code process.
// Manager assembles these (including pre-built CLI args) before calling
// SessionBackend.StartProcess, keeping arg-construction logic out of the backend.
type ProcessStartOpts struct {
	Command string   // claude binary path (e.g., "/usr/local/bin/claude")
	WorkDir string   // working directory for the process
	Args    []string // fully assembled CLI args (e.g., ["--resume", "<id>", "--agent", "foo"])
	Env     []string // additional KEY=VALUE pairs appended to the process environment
}
