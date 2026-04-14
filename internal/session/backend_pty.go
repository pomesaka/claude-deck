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
// The onExit callback is fired (in a goroutine) after <-proc.Done() returns,
// i.e., after the process has fully exited. Manager wires this to its
// watchProcess method, which handles session state transitions and
// persistence — concerns that belong in Manager, not here.
type ptyBackend struct {
	sup    *ProcessSupervisor
	onExit func(sess *Session)
}

// newPTYBackend creates a ptyBackend backed by the given supervisor.
// onExit is called in a dedicated goroutine after the process exits.
func newPTYBackend(sup *ProcessSupervisor, onExit func(sess *Session)) *ptyBackend {
	return &ptyBackend{sup: sup, onExit: onExit}
}

// StartProcess launches a Claude Code process in a pseudo-terminal.
// opts.Args are pre-assembled by Manager; they map directly to pty.StartOptions.AdditionalArgs.
//
// AttachProcess is called with the display BEFORE the output goroutine starts
// (i.e., before pty.Start's internal read goroutine can fire onOutput), so
// early PTY output is never discarded to a nil process/display.
// SetPID is called immediately after pty.Start to fill in the real PID.
func (b *ptyBackend) StartProcess(ctx context.Context, sess *Session, opts ProcessStartOpts, onOutput func([]byte)) error {
	// Create display and attach BEFORE pty.Start so that the output goroutine
	// (started inside pty.Start) always finds a non-nil process with a display.
	display := sess.NewDisplay(int(opts.Cols), int(opts.Rows), opts.MaxScrollback)
	sess.AttachProcess(0, display) // PID=0 placeholder; SetPID called after pty.Start

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
		sess.DetachProcess() // clean up the display we just attached
		return err
	}

	sess.SetPID(proc.PID())
	b.sup.Register(sess.ID, proc)

	// Spawn exit-watcher: block until process exits, then fire onExit.
	// <-proc.Done() is here (not in Manager.watchProcess) so that tmuxBackend
	// can share the same onExit signature — it has no *pty.Process to pass.
	go func() {
		<-proc.Done()
		if b.onExit != nil {
			b.onExit(sess)
		}
	}()

	return nil
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

func (b *ptyBackend) Focus(_ DeckSessionID) error {
	// ptyBackend: no-op. The TUI manages which PTY viewport is visible
	// internally; no external terminal command is needed.
	return nil
}

func (b *ptyBackend) EnsurePreview() error { return nil }
func (b *ptyBackend) FocusPreview() error  { return nil }
func (b *ptyBackend) KillPreview() error   { return nil }

func (b *ptyBackend) Reconcile(_ []*Session) (ReconcileResult, error) {
	// PTY processes are tracked individually via ProcessSupervisor.
	// No external process state to reconcile.
	return ReconcileResult{}, nil
}

func (b *ptyBackend) Close() {
	// ProcessSupervisor has no cleanup needed; PTY processes are managed
	// individually via Kill/WaitForExit. Nothing to release here.
}
