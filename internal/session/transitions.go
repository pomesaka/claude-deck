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

// closingTimeout is how long a close in progress blocks other operations on the row.
// WHY 期限付き: close の途中でプロセスが落ちると ClosingAt が残り、そのセッションを二度と close できなくなる。
// ワークスペースの削除（jj workspace forget + RemoveAll）は通常数秒で終わるので、それより十分長くとる。
const closingTimeout = 2 * time.Minute

// launchTimeout is how long a launch in progress protects the row from being
// marked exited for lacking a tmux window.
// WHY 期限付き: 起動の途中でプロセスが落ちると LaunchingAt が残り、ウィンドウの無い行が終了扱いにならない。
// 起動（jj ワークスペース作成 + tmux new-window）は通常数秒で終わるので、それより十分長くとる。
const launchTimeout = 2 * time.Minute

// closingActive reports whether a close started less than closingTimeout ago.
func closingActive(r store.Record, now time.Time) bool {
	return r.ClosingAt != nil && now.Sub(*r.ClosingAt) < closingTimeout
}

// launchActive reports whether a launch started less than launchTimeout ago.
func launchActive(r store.Record, now time.Time) bool {
	return r.LaunchingAt != nil && now.Sub(*r.LaunchingAt) < launchTimeout
}

// ErrClosing is returned when another process is already closing the session.
var ErrClosing = errors.New("session is already being closed")

// ErrLaunching is returned when another process is still starting the session.
var ErrLaunching = errors.New("session is still being launched")

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
	// 終了したプロセスの PID を残すと、後の close が再利用された別プロセスに SIGTERM を送りうる。
	r.PID = 0
	r.LaunchingAt = nil
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

// vanished reports whether the row should be marked exited because its tmux
// window is gone: it is unfinished, and no close or launch is in progress.
// Callers must read the row inside the same transaction that writes the exit,
// so a launch that started after the window list was taken is seen here.
func vanished(r store.Record, now time.Time) bool {
	status, ok := StatusFromID(r.Status)
	if !ok || status.IsTerminal() || status == StatusUnmanaged {
		return false
	}
	return !closingActive(r, now) && !launchActive(r, now)
}

// beginClose marks the row as being closed. It fails while another close or a
// launch is in progress: closing a session whose window is about to appear would
// delete its workspace under the starting process.
func beginClose(r *store.Record, now time.Time) error {
	if closingActive(*r, now) {
		return fmt.Errorf("%w: %s", ErrClosing, r.ID)
	}
	if launchActive(*r, now) {
		return fmt.Errorf("%w: %s", ErrLaunching, r.ID)
	}
	r.ClosingAt = &now
	return nil
}

// beginLaunch marks the row as being launched. Insert a new row with it set, or
// call beginResume, before starting the process.
func beginLaunch(r *store.Record, now time.Time) {
	r.LaunchingAt = &now
	r.PID = 0
}

// finishLaunch records the started process and ends the launch.
// pid may be 0 when tmux could not report it; the launch is over either way.
func finishLaunch(r *store.Record, pid int) {
	r.PID = pid
	r.LaunchingAt = nil
}

// beginResume moves a finished session back to Idle before its process starts.
// It fails unless the session is finished, so two resumes of the same session
// cannot both start a process.
func beginResume(r *store.Record, now time.Time) error {
	status, ok := StatusFromID(r.Status)
	if !ok || !status.IsTerminal() {
		return fmt.Errorf("session %s is not finished (status %s)", r.ID, r.Status)
	}
	if closingActive(*r, now) {
		return fmt.Errorf("%w: %s", ErrClosing, r.ID)
	}
	r.Status = StatusIdle.ID()
	r.FinishedAt = nil
	r.ErrorMessage = ""
	r.ClosingAt = nil
	beginLaunch(r, now)
	return nil
}

// abortResume returns a row whose resume could not start a process to Completed.
func abortResume(r *store.Record, now time.Time) {
	r.LaunchingAt = nil
	r.Status = StatusCompleted.ID()
	r.FinishedAt = &now
}

// markAdopted turns the row built from an external session into a finished deck
// session, so beginResume accepts it.
func markAdopted(r *store.Record, now time.Time) {
	r.Status = StatusCompleted.ID()
	r.FinishedAt = &now
}

// reviveForLiveWindow sets a finished row back to Idle because its tmux window,
// running as pid, is alive. A row that is not finished is left as is.
func reviveForLiveWindow(r *store.Record, pid int) (changed bool) {
	status, ok := StatusFromID(r.Status)
	if !ok || !status.IsTerminal() {
		return false
	}
	r.Status = StatusIdle.ID()
	r.FinishedAt = nil
	r.ErrorMessage = ""
	r.ClosingAt = nil
	finishLaunch(r, pid)
	return true
}

// recordWorkspaceRemoved clears the workspace a close has just removed and
// records where its working copy was, for the resume to recreate it (ADR 009).
// atRev and parentRev are both empty when they could not be read: an older pair
// must not be used for a workspace that has moved on since.
// WHY 消したときだけ呼ぶ: ワークスペースの無い行（すでに close 済み）をもう一度 close しても
// 保存済みの位置は変わらない。そこで空を書くと、resume が trunk() からの作り直しになる。
func recordWorkspaceRemoved(r *store.Record, atRev, parentRev string) {
	r.WorkspaceName = ""
	r.WorkspacePath = ""
	r.LastJJRevision, r.LastJJParentRevision = atRev, parentRev
}

// setError marks the row as failed with a reason.
func setError(r *store.Record, msg string, now time.Time) {
	r.Status = StatusError.ID()
	r.ErrorMessage = msg
	if r.FinishedAt == nil {
		r.FinishedAt = &now
	}
}
