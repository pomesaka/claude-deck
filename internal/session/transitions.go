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
// 既存の行の Status を書き換えるのはこのファイルの setStatus だけにする（rg 'r\.Status = ' internal/session で確かめる）。

// applyHookStatus sets the activity reported by a hook (or the Codex JSONL).
// Activities only come from a live process, so a row that is not live is left
// as is: a hook delivered after the exit was recorded must not bring it back,
// and a status this version does not know is not ours to overwrite.
//
// w is kept only with ActivityWaitingAction; any other activity clears it.
// The same status with a new wait is a change: the list shows the reason.
func applyHookStatus(r *store.Record, a Activity, w Wait) (changed bool) {
	current, _ := StatusFromID(r.Status)
	if !current.IsLive() {
		return false
	}
	if a != ActivityWaitingAction {
		w = Wait{}
	}
	if current == a.Status() && r.StatusReason == w.Reason && r.WaitKind == string(w.Kind) {
		return false
	}
	setStatus(r, a.Status())
	r.StatusReason = w.Reason
	r.WaitKind = string(w.Kind)
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

// applyContextOnSessionStart updates what Claude Code measured of the
// conversation when SessionStart reports how it began.
//
//   - startup of a session with no chain, clear: a new conversation, with no
//     compaction and no fill yet.
//   - compact: one more compaction, and the fill is unknown until the next response.
//   - fork: the conversation is another session's, whose compactions this row
//     does not know.
//   - resume, and startup of a known conversation: the conversation goes on.
//
// A row that is not live is left as is: a hook delivered after the exit was recorded.
//
// WHY store の側で数える: compaction の後の SessionStart（source が compact）は、今の deckmod より
// 前の版も送っている。数える場所をここ 1 か所にでき、Mod の読み込み直しで数が失われない。
// WHY NOT chain で重複を捨てる: compaction の後もセッション ID は変わらない（6 回 compaction した
// 記録の sessionId が 1 つだけだった、2026-11-28）。同じ SessionStart が 2 回届けば 2 回数える。
// 呼ぶのは applySessionStart の前（startup が新しい会話かを、更新前の chain で見る）。
func applyContextOnSessionStart(r *store.Record, source string) {
	if status, _ := StatusFromID(r.Status); !status.IsLive() {
		return
	}
	switch source {
	case SourceStartup:
		if len(r.SessionChain) > 0 {
			return
		}
		r.Compactions = new(0)
		r.ContextTokens = nil
	case SourceClear:
		r.Compactions = new(0)
		r.ContextTokens = nil
	case SourceCompact:
		if r.Compactions != nil {
			r.Compactions = new(*r.Compactions + 1)
		}
		r.ContextTokens = nil
	case SourceFork:
		r.Compactions = nil
		r.ContextTokens = nil
	}
}

// markActive records that the session's process reported something through a
// hook at now. A row that is not live is left as is: a hook delivered after the
// exit was recorded did not come from a moving session.
// WHY 新しい列: store の last_activity は TUI が終わるときにしか書かれず（sortTimeOf の WHY）、
// TUI を動かしたままの CLI の list からは、動いているセッションも古く見える。hook はターンの
// 始まりと終わりごとに store を書くので、その時刻を写す。
func markActive(r *store.Record, now time.Time) {
	if status, _ := StatusFromID(r.Status); !status.IsLive() {
		return
	}
	r.ActiveAt = &now
}

// applyContextMeasured records the context window that session.measure
// reported. tokens is nil until a response of the live window reports a fill.
func applyContextMeasured(r *store.Record, tokens *int, window int) {
	r.ContextWindow = new(window)
	r.ContextTokens = nil
	if tokens != nil {
		r.ContextTokens = new(*tokens)
	}
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
		setStatus(r, StatusCompleted)
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
	if status, _ := StatusFromID(r.Status); !status.IsLive() {
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
	setStatus(r, StatusIdle)
	r.FinishedAt = nil
	r.ErrorMessage = ""
	r.ClosingAt = nil
	beginLaunch(r, now)
	return nil
}

// abortResume returns a row whose resume could not start a process to Completed.
func abortResume(r *store.Record, now time.Time) {
	r.LaunchingAt = nil
	setStatus(r, StatusCompleted)
	r.FinishedAt = &now
}

// markAdopted turns the row built from an external session into a finished deck
// session, so beginResume accepts it.
func markAdopted(r *store.Record, now time.Time) {
	setStatus(r, StatusCompleted)
	r.FinishedAt = &now
}

// reviveForLiveWindow sets a finished row back to Idle because its tmux window,
// running as pid, is alive. A row that is not finished is left as is.
func reviveForLiveWindow(r *store.Record, pid int) (changed bool) {
	status, ok := StatusFromID(r.Status)
	if !ok || !status.IsTerminal() {
		return false
	}
	setStatus(r, StatusIdle)
	r.FinishedAt = nil
	r.ErrorMessage = ""
	r.ClosingAt = nil
	finishLaunch(r, pid)
	return true
}

// recordWorkspaceRemoved clears the workspace a close has just removed and
// records where its working copy was, for the resume to recreate it.
// atRev and parentRev are both empty when they could not be read: an older pair
// must not be used for a workspace that has moved on since.
// WHY 消したときだけ呼ぶ: ワークスペースの無い行（すでに close 済み）をもう一度 close しても
// 保存済みの位置は変わらない。そこで空を書くと、resume が trunk() からの作り直しになる。
func recordWorkspaceRemoved(r *store.Record, atRev, parentRev string) {
	isolationToRecord(r, isolationFromRecord(*r).Removed(atRev, parentRev))
}

// recordWorkspaceRecreated records the workspace a resume made again, with the
// process to run in workDir.
func recordWorkspaceRecreated(r *store.Record, workDir string) {
	isolationToRecord(r, WorkspaceIsolation(workDir))
}

// setError marks the row as failed with a reason.
func setError(r *store.Record, msg string, now time.Time) {
	setStatus(r, StatusError)
	r.ErrorMessage = msg
	if r.FinishedAt == nil {
		r.FinishedAt = &now
	}
}

// setStatus moves the row to s. A reason belongs to the wait it was given for,
// so every move clears it and its kind; applyHookStatus sets them again for WaitingAction.
func setStatus(r *store.Record, s Status) {
	r.Status = s.ID()
	r.StatusReason = ""
	r.WaitKind = ""
}
