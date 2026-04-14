package session

import "context"

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
	// onOutput is called for each raw PTY output chunk. The backend registers
	// the process and fires the onExit callback when the process exits.
	// Returns the OS process ID on success.
	StartProcess(ctx context.Context, sess *Session, opts ProcessStartOpts, onOutput func([]byte)) (pid int, err error)

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

	// Close releases all resources held by the backend.
	Close()
}

// ProcessStartOpts contains all parameters needed to start a Claude Code process.
// Manager assembles these (including pre-built CLI args) before calling
// SessionBackend.StartProcess, keeping arg-construction logic out of the backend.
// Each backend decides how to execute — PTY, tmux window, Ghostty split, etc.
type ProcessStartOpts struct {
	Command string   // claude binary path (e.g., "/usr/local/bin/claude")
	WorkDir string   // working directory for the process
	Args    []string // fully assembled CLI args (e.g., ["--resume", "<id>", "--agent", "foo"])
	Env     []string // additional KEY=VALUE pairs appended to the process environment
	Cols    uint16   // terminal width; 0 → backend default
	Rows    uint16   // terminal height; 0 → backend default
}
