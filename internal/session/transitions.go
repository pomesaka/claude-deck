package session

import (
	"errors"
	"fmt"
	"time"

	"github.com/pomesaka/claude-deck/internal/store"
)

// SessionStart sources reported by Claude Code.
const (
	SourceStartup = "startup"
	SourceResume  = "resume"
	SourceClear   = "clear"
	SourceCompact = "compact"
	// SourceFork is reported by a process started with --resume <id> --fork-session.
	SourceFork = "fork"
)

// closingTimeout is how long a close in progress blocks another close.
// WHY 期限付き: close の途中でプロセスが落ちると ClosingAt が残り、そのセッションを二度と close できなくなる。
// ワークスペースの削除（jj workspace forget + RemoveAll）は通常数秒で終わるので、それより十分長くとる。
const closingTimeout = 2 * time.Minute

// ErrClosing is returned when another process is already closing the session.
var ErrClosing = errors.New("session is already being closed")

// The functions below are the state transitions on a store row. Every process
// (TUI, CLI, hook commands, the pane's exit command) applies them inside a
// store transaction, so the rules live here rather than in any one process.

// applyHookStatus sets the status reported by a Claude Code hook.
// Hooks only come from a live process, so a terminal or external session is left
// as is: a hook delivered after the exit was recorded must not bring it back.
func applyHookStatus(r *store.Record, status Status) (changed bool) {
	current, ok := StatusFromID(r.Status)
	if !ok || current.IsTerminal() || current == StatusUnmanaged {
		return false
	}
	if current == status {
		return false
	}
	r.Status = status.ID()
	return true
}

// applySessionStart links the Claude Code session ID reported by SessionStart.
//
//   - startup / resume / fork: the first ID of a deck session. Ignored once a chain
//     exists, because resuming reports the ID the chain already ends with.
//   - clear / compact: Claude Code switched to a new ID; append it.
func applySessionStart(r *store.Record, claudeID, source string) (changed bool) {
	if claudeID == "" {
		return false
	}
	switch source {
	case SourceStartup, SourceResume, SourceFork:
		if len(r.SessionChain) > 0 {
			return false
		}
	case SourceClear, SourceCompact:
		if len(r.SessionChain) > 0 && r.SessionChain[len(r.SessionChain)-1] == claudeID {
			return false
		}
	default:
		return false
	}
	r.SessionChain = append(r.SessionChain, claudeID)
	return true
}

// applyExited records that the session's process has ended.
//
// If the session was /clear-ed and then exited before any message was sent, the
// newest Claude session has no conversation and cannot be resumed; the chain
// falls back to the previous ID. The fallback is skipped when another deck
// session already owns that ID (it was imported as an external session), so two
// deck sessions never share a Claude session ID.
func applyExited(r *store.Record, others []store.Record, hasConversation func(claudeID string) bool, now time.Time) {
	if status, ok := StatusFromID(r.Status); !ok || !status.IsTerminal() {
		r.Status = StatusCompleted.ID()
		r.FinishedAt = &now
	}
	if len(r.SessionChain) < 2 {
		return
	}
	newest := r.SessionChain[len(r.SessionChain)-1]
	prev := r.SessionChain[len(r.SessionChain)-2]
	if hasConversation(newest) {
		return
	}
	for _, o := range others {
		if o.ID != r.ID && len(o.SessionChain) > 0 && o.SessionChain[len(o.SessionChain)-1] == prev {
			return
		}
	}
	r.SessionChain = r.SessionChain[:len(r.SessionChain)-1]
}

// beginClose marks the row as being closed. It fails if another close started
// less than closingTimeout ago.
func beginClose(r *store.Record, now time.Time) error {
	if r.ClosingAt != nil && now.Sub(*r.ClosingAt) < closingTimeout {
		return fmt.Errorf("%w: %s", ErrClosing, r.ID)
	}
	r.ClosingAt = &now
	return nil
}

// beginResume moves a finished session back to Idle before its process starts.
// It fails unless the session is finished, so two resumes of the same session
// cannot both start a process.
//
// PID is cleared until the new process reports its PID: the TUI treats an
// unfinished session with PID 0 as "launch in progress" and does not mark it
// exited for lacking a tmux window.
func beginResume(r *store.Record) error {
	status, ok := StatusFromID(r.Status)
	if !ok || !status.IsTerminal() {
		return fmt.Errorf("session %s is not finished (status %s)", r.ID, r.Status)
	}
	if r.ClosingAt != nil {
		return fmt.Errorf("%w: %s", ErrClosing, r.ID)
	}
	r.Status = StatusIdle.ID()
	r.FinishedAt = nil
	r.ErrorMessage = ""
	r.PID = 0
	return nil
}

// setError marks the row as failed with a reason.
func setError(r *store.Record, msg string, now time.Time) {
	r.Status = StatusError.ID()
	r.ErrorMessage = msg
	if r.FinishedAt == nil {
		r.FinishedAt = &now
	}
}
