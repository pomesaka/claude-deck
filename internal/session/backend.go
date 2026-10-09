package session

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
//   - Recording that a process exited: the command passed in ProcessStartOpts.OnExit
//     does that from inside the hosting environment (ADR-011)
type SessionBackend interface {
	// StartProcess launches a Claude Code process for the session and returns its PID.
	StartProcess(sessionID DeckSessionID, opts ProcessStartOpts) (pid int, err error)

	// StopProcess terminates the process for the given session.
	// fallbackPID is signalled when the hosting window is already gone.
	StopProcess(sessionID DeckSessionID, fallbackPID int) error

	// IsActive returns true if the session has a live, non-exited process.
	IsActive(sessionID DeckSessionID) bool

	// LiveSessions returns the sessions that have a live hosting window, with the
	// window's process PID.
	LiveSessions() (map[DeckSessionID]int, error)

	// KillOrphans stops hosting windows that belong to no known session.
	KillOrphans(known map[DeckSessionID]bool) error

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
}

// ProcessStartOpts contains all parameters needed to start a Claude Code process.
// Manager assembles these (including pre-built CLI args) before calling
// SessionBackend.StartProcess, keeping arg-construction logic out of the backend.
type ProcessStartOpts struct {
	Command string   // claude binary path (e.g., "/usr/local/bin/claude")
	WorkDir string   // working directory for the process
	Args    []string // fully assembled CLI args (e.g., ["--resume", "<id>", "--name", "foo"])
	Env     []string // additional KEY=VALUE pairs appended to the process environment
	// OnExit is a command run in the same hosting window after the process exits
	// (on its own, by crash, or by a signal to the process). It does not run when
	// the window itself is killed (StopProcess). Empty means nothing runs.
	OnExit []string
}
