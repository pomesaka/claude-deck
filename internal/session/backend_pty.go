package session

import (
	"context"

	"github.com/pomesaka/claude-deck/internal/pty"
)

// Compile-time interface satisfaction check.
var _ SessionBackend = (*ptyBackend)(nil)

// ptyBackend implements SessionBackend for embedded PTY hosting.
// It delegates process tracking to ProcessSupervisor and calls pty.Start
// to launch Claude Code in a pseudo-terminal managed by claude-deck.
//
// The onExit callback is fired (in a goroutine) when the process exits.
// Manager wires this to its watchProcess method, which handles session state
// transitions and persistence — concerns that belong in Manager, not here.
type ptyBackend struct {
	sup    *ProcessSupervisor
	onExit func(sess *Session, proc *pty.Process)
}

// newPTYBackend creates a ptyBackend backed by the given supervisor.
// onExit is called in a dedicated goroutine when a process exits.
func newPTYBackend(sup *ProcessSupervisor, onExit func(sess *Session, proc *pty.Process)) *ptyBackend {
	return &ptyBackend{sup: sup, onExit: onExit}
}

// StartProcess launches a Claude Code process in a pseudo-terminal.
// opts.Args are pre-assembled by Manager; they map directly to pty.StartOptions.AdditionalArgs.
// The process is registered with the supervisor and an exit-watcher goroutine is spawned.
func (b *ptyBackend) StartProcess(ctx context.Context, sess *Session, opts ProcessStartOpts, onOutput func([]byte)) (int, error) {
	proc, err := pty.Start(ctx, pty.StartOptions{
		Command:        opts.Command,
		WorkDir:        opts.WorkDir,
		AdditionalArgs: opts.Args,
		Env:            opts.Env,
		Cols:           opts.Cols,
		Rows:           opts.Rows,
		// ResumeSessionID / ForkSession / Prompt / PermissionMode are intentionally
		// left empty: Manager pre-assembles those flags into opts.Args so that the
		// backend interface stays decoupled from PTY-specific flag semantics.
	}, onOutput)
	if err != nil {
		return 0, err
	}

	b.sup.Register(sess.ID, proc)

	// Spawn exit-watcher: blocks until process exits, then fires onExit.
	// Manager.watchProcess (wired as onExit) handles all post-exit domain logic.
	go func() {
		if b.onExit != nil {
			b.onExit(sess, proc)
		}
	}()

	return proc.PID(), nil
}

func (b *ptyBackend) StopProcess(sessionID DeckSessionID, fallbackPID int) error {
	return b.sup.Kill(sessionID, fallbackPID)
}

func (b *ptyBackend) IsActive(sessionID DeckSessionID) bool {
	return b.sup.IsAlive(sessionID)
}

func (b *ptyBackend) WriteInput(sessionID DeckSessionID, data []byte) error {
	return b.sup.Write(sessionID, data)
}

func (b *ptyBackend) Resize(sessionID DeckSessionID, cols, rows uint16) {
	b.sup.Resize(sessionID, cols, rows)
}

func (b *ptyBackend) Close() {
	// ProcessSupervisor has no cleanup needed; PTY processes are managed
	// individually via Kill/WaitForExit. Nothing to release here.
}
