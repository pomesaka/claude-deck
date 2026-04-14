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
// It decouples Manager from the concrete mechanism used to host processes (PTY,
// tmux window, Ghostty split, etc.), enabling backend swapping without touching
// session domain logic.
//
// Responsibilities:
//   - Process lifecycle: start, stop, liveness check
//   - I/O: send input, resize terminal
//
// Non-responsibilities (stay in Manager/Session):
//   - Session object creation and persistence
//   - jj workspace management
//   - JSONL streaming and status tracking
//   - Display data (PTY lines, cursor) — exposed via Session methods directly
type SessionBackend interface {
	// StartProcess launches a Claude Code process for the session.
	// The backend is responsible for:
	//   - creating and attaching a RunningProcess to sess (via sess.AttachProcess)
	//   - wiring output capture for Embedded backends (PTY display)
	//   - spawning the exit-watcher goroutine that calls the onExit callback
	//
	// For Embedded (PTY) backends: AttachProcess is called BEFORE the output goroutine
	// starts, so early PTY output is never lost to a nil display.
	// For External (tmux) backends: AttachProcess is called with nil display.
	//
	// onOutput is called for each raw PTY output chunk (Embedded backends only;
	// External backends ignore it — they have no PTY output to capture).
	StartProcess(ctx context.Context, sess *Session, opts ProcessStartOpts, onOutput func([]byte)) error

	// StopProcess terminates the process for the given session.
	// fallbackPID is used when the process handle is unavailable (e.g., session
	// restored from store without a live handle).
	StopProcess(sessionID DeckSessionID, fallbackPID int) error

	// IsActive returns true if the session has a live, non-exited process.
	IsActive(sessionID DeckSessionID) bool

	// WriteInput sends raw bytes to the process's standard input.
	WriteInput(sessionID DeckSessionID, data []byte) error

	// Resize updates the terminal window dimensions reported to the process.
	// Called when the TUI viewport size changes.
	Resize(sessionID DeckSessionID, cols, rows uint16)

	// Focus makes the session's terminal visible in the hosting environment.
	// ptyBackend: no-op (the TUI manages the PTY viewport internally).
	// tmuxBackend: runs tmux select-window for ~0ms session switching.
	Focus(sessionID DeckSessionID) error

	// EnsurePreview creates the preview subprocess window if not already present.
	// The preview window displays JSONL logs for the selected session in split mode.
	// ptyBackend: no-op.
	// tmuxBackend: creates/verifies the __preview__ tmux window.
	EnsurePreview() error

	// FocusPreview switches the display to the preview window.
	// ptyBackend: no-op.
	// tmuxBackend: runs tmux select-window __preview__.
	FocusPreview() error

	// KillPreview destroys the preview window.
	// ptyBackend: no-op.
	// tmuxBackend: kills the __preview__ tmux window.
	KillPreview() error

	// Reconcile synchronises backend state against the provided session list.
	// For each session with a live backend process, the backend re-attaches its
	// exit watcher and includes the session ID in ReconcileResult.LivePIDs.
	// Orphaned backend processes (no corresponding deck session) are killed.
	// ptyBackend: no-op — PTY processes are tracked per-session by ProcessSupervisor.
	// tmuxBackend: reconciles tmux windows against the session list.
	Reconcile(sessions []*Session) (ReconcileResult, error)

	// Close releases all resources held by the backend.
	Close()
}

// ProcessStartOpts contains all parameters needed to start a Claude Code process.
// Manager assembles these (including pre-built CLI args) before calling
// SessionBackend.StartProcess, keeping arg-construction logic out of the backend.
// Each backend decides how to execute — PTY, tmux window, Ghostty split, etc.
type ProcessStartOpts struct {
	Command       string   // claude binary path (e.g., "/usr/local/bin/claude")
	WorkDir       string   // working directory for the process
	Args          []string // fully assembled CLI args (e.g., ["--resume", "<id>", "--agent", "foo"])
	Env           []string // additional KEY=VALUE pairs appended to the process environment
	Cols          uint16   // terminal width; 0 → backend default
	Rows          uint16   // terminal height; 0 → backend default
	MaxScrollback int      // PTY scrollback buffer size; 0 → backend default (used by Embedded backends)
}
