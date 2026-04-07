package session

import (
	"fmt"
	"os"
	"sync"
	"syscall"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/pty"
)

// ProcessSupervisor manages the lifecycle of PTY processes independently
// from session domain logic. It tracks which sessions have active processes,
// handles I/O routing, and monitors process termination.
//
// Separating this from Manager ensures that PTY infrastructure concerns
// (process start/stop, I/O, resize) don't pollute session lifecycle logic,
// and changes to PTY management don't affect session domain code.
type ProcessSupervisor struct {
	mu        sync.RWMutex
	processes map[string]*pty.Process // deck session ID → PTY process
}

// NewProcessSupervisor creates a new supervisor.
func NewProcessSupervisor() *ProcessSupervisor {
	return &ProcessSupervisor{
		processes: make(map[string]*pty.Process),
	}
}

// Register associates a PTY process with a session ID.
func (ps *ProcessSupervisor) Register(sessionID string, proc *pty.Process) {
	ps.mu.Lock()
	ps.processes[sessionID] = proc
	ps.mu.Unlock()
}

// Unregister removes the process association for a session ID.
func (ps *ProcessSupervisor) Unregister(sessionID string) {
	ps.mu.Lock()
	delete(ps.processes, sessionID)
	ps.mu.Unlock()
}

// Get returns the PTY process for a session, or nil if not found.
func (ps *ProcessSupervisor) Get(sessionID string) *pty.Process {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.processes[sessionID]
}

// IsAlive returns true if the session has a PTY process that hasn't exited.
func (ps *ProcessSupervisor) IsAlive(sessionID string) bool {
	ps.mu.RLock()
	proc, ok := ps.processes[sessionID]
	ps.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case <-proc.Done():
		return false
	default:
		return true
	}
}

// Write sends data to the PTY stdin of a session.
func (ps *ProcessSupervisor) Write(sessionID string, data []byte) error {
	proc := ps.Get(sessionID)
	if proc == nil {
		return fmt.Errorf("no active process for session %s", sessionID)
	}
	if _, err := proc.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// Resize updates the PTY window size for a session.
func (ps *ProcessSupervisor) Resize(sessionID string, cols, rows uint16) {
	proc := ps.Get(sessionID)
	if proc == nil {
		return
	}
	proc.Resize(cols, rows) //nolint:errcheck
}

// Kill forcefully terminates a session's PTY process.
// If the process handle is not available but a PID is provided, falls back to SIGTERM.
func (ps *ProcessSupervisor) Kill(sessionID string, fallbackPID int) error {
	proc := ps.Get(sessionID)
	if proc != nil {
		return proc.Kill()
	}

	// プロセスハンドルなし（前回起動時のセッションがストアから復元された場合など）。
	if fallbackPID > 0 {
		if p, err := os.FindProcess(fallbackPID); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
	return nil
}

// ActiveSessionIDs returns the IDs of sessions with live processes.
func (ps *ProcessSupervisor) ActiveSessionIDs() []string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	ids := make([]string, 0, len(ps.processes))
	for id, proc := range ps.processes {
		select {
		case <-proc.Done():
			continue
		default:
			ids = append(ids, id)
		}
	}
	return ids
}

// WaitForExit blocks until the process for the given session exits.
// Returns immediately if no process is registered.
func (ps *ProcessSupervisor) WaitForExit(sessionID string) {
	proc := ps.Get(sessionID)
	if proc == nil {
		return
	}
	<-proc.Done()
	debuglog.Printf("[supervisor] process exited session=%s", sessionID)
}
