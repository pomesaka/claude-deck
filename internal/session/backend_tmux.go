package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/tmux"
)

// previewWindowName is the tmux window reserved for the preview subprocess.
// The window runs "claude-deck --preview" and shows JSONL logs for the session
// selected in the list TUI.  Kept unexported — callers use EnsurePreview/FocusPreview/KillPreview.
const previewWindowName = "__preview__"

// Compile-time interface satisfaction check.
var _ SessionBackend = (*tmuxBackend)(nil)

// tmuxBackend implements SessionBackend using tmux windows as the process
// hosting mechanism.  Each Claude Code session maps to one tmux window.
// Switching between sessions is done via tmux select-window (~0ms).
//
// Claude Code runs natively in tmux — no PTY emulation, no output capture.
// Metadata updates (status, tokens, etc.) come entirely from JSONL and hook events.
//
// ctx is the application-lifetime context. Exit-watcher goroutines use it so they
// can terminate cleanly when the application shuts down.
//
// The tmux session is persistent: it survives claude-deck restarts and users
// can detach/reattach freely via `tmux attach -t <session>`.
type tmuxBackend struct {
	ctx    context.Context
	runner *tmux.Runner
	onExit func(sess *Session)
}

// newTmuxBackend creates a tmuxBackend using the given runner.
// ctx should be the application-lifetime context so exit-watcher goroutines shut
// down cleanly when the application exits.
// onExit is called in a dedicated goroutine after the window's pane exits.
func newTmuxBackend(ctx context.Context, runner *tmux.Runner, onExit func(sess *Session)) *tmuxBackend {
	return &tmuxBackend{ctx: ctx, runner: runner, onExit: onExit}
}

// StartProcess creates a tmux window for the session and starts Claude Code in it.
// onOutput is intentionally ignored — no PTY output is captured; users interact directly.
// AttachProcess(pid) is called before the exit-watcher goroutine starts so that
// Session.process is non-nil for the window's lifetime.
//
// Exit detection relies on tmux's pane-exited hook wired to a wait-for channel.
// A race-guard in the exit-watcher goroutine handles the case where the pane
// exits before WaitFor begins.
func (b *tmuxBackend) StartProcess(ctx context.Context, sess *Session, opts ProcessStartOpts, _ func([]byte)) error {
	windowName := string(sess.ID)
	channel := tmux.ExitChannel(windowName)

	// Build a shell-safe command string from the binary and pre-assembled args.
	shellCmd := buildShellCommand(opts.Command, opts.Args)
	debuglog.Printf("[tmuxBackend] StartProcess session=%s cmd=%s", sess.ID, shellCmd)

	wopts := tmux.WindowOpts{
		Command: shellCmd,
		WorkDir: opts.WorkDir,
		Env:     opts.Env,
	}
	if err := b.runner.NewWindow(windowName, wopts); err != nil {
		// The tmux session may have been destroyed (e.g., last window killed).
		// Re-create it and retry once before giving up.
		debuglog.Printf("[tmuxBackend] new-window failed (session gone?), re-creating session and retrying: %v", err)
		if rerr := b.runner.NewSession(); rerr != nil {
			return fmt.Errorf("tmux new-window: %w (session re-create also failed: %v)", err, rerr)
		}
		b.runner.ApplyDefaultOptions()
		if rerr := b.runner.NewWindow(windowName, wopts); rerr != nil {
			return fmt.Errorf("tmux new-window: %w", rerr)
		}
	}

	// Wire the pane-exited hook so we know when Claude Code finishes.
	// The hook fires a wait-for signal that unblocks the exit-watcher goroutine below.
	// channel は ExitChannel(windowName) で生成され、windowName は DeckSessionID（hex 文字列）由来。
	// tmux の wait-for はチャネル名にスペースや特殊文字を許容しないが、hex 文字列はそれらを含まないため安全。
	// （これは tmux コマンド構文の安全性であり、shell エスケープとは別の話。）
	hookCmd := "wait-for -S " + channel
	hookOK := true
	if err := b.runner.SetHook(windowName, "pane-exited", hookCmd); err != nil {
		debuglog.Printf("[tmuxBackend] SetHook failed session=%s: %v — falling back to polling", sess.ID, err)
		hookOK = false
	}

	// Read the pane PID and attach before the exit-watcher goroutine starts —
	// session.process is non-nil for the window's lifetime.
	pid, _ := b.runner.PanePID(windowName)
	sess.AttachProcess(pid)

	// Exit-watcher goroutine: blocks until the pane exits, then fires onExit.
	// Race guard: check HasWindow before WaitFor — if the pane already exited
	// (process completed before SetHook could register the hook), fire immediately.
	// When SetHook failed, fall back to polling HasWindow so the goroutine never leaks.
	// b.ctx cancellation (app shutdown) exits both paths cleanly.
	appCtx := b.ctx
	go func() {
		if !b.runner.HasWindow(windowName) {
			debuglog.Printf("[tmuxBackend] pane already gone before wait-for session=%s", sess.ID)
		} else if hookOK {
			// Blocks until the pane-exited hook calls "wait-for -S <channel>", or appCtx is cancelled.
			if err := b.runner.WaitForCtx(appCtx, channel); err != nil {
				debuglog.Printf("[tmuxBackend] wait-for ended session=%s: %v", sess.ID, err)
				if appCtx.Err() != nil {
					return
				}
			} else {
				debuglog.Printf("[tmuxBackend] pane exited session=%s", sess.ID)
			}
		} else {
			// SetHook failed: poll until the window disappears or appCtx is cancelled.
			// time.NewTimer instead of time.After to avoid per-iteration timer leaks.
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()
			for b.runner.HasWindow(windowName) {
				select {
				case <-appCtx.Done():
					return
				case <-timer.C:
					timer.Reset(2 * time.Second)
				}
			}
			debuglog.Printf("[tmuxBackend] pane exited (polled) session=%s", sess.ID)
		}
		if b.onExit != nil {
			b.onExit(sess)
		}
	}()

	return nil
}

// StopProcess kills the tmux window, stopping the Claude Code process inside.
// If KillWindow fails (e.g., window already gone), falls back to SIGTERM on PID.
func (b *tmuxBackend) StopProcess(sessionID DeckSessionID, fallbackPID int) error {
	windowName := string(sessionID)
	if err := b.runner.KillWindow(windowName); err == nil {
		return nil
	}
	// KillWindow failed — likely the window is already gone (process exited).
	// Try SIGTERM on the stored PID as a best-effort fallback.
	if fallbackPID <= 0 {
		return nil
	}
	proc, err := os.FindProcess(fallbackPID)
	if err != nil {
		return nil // process doesn't exist
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// IsActive returns true if the tmux window for the session still exists.
func (b *tmuxBackend) IsActive(sessionID DeckSessionID) bool {
	return b.runner.HasWindow(string(sessionID))
}

// Focus selects the session's window in the tmux session, making it visible
// in any attached tmux client.  This is called on session list navigation for
// near-instant (~0ms) switching between Claude Code sessions.
func (b *tmuxBackend) Focus(sessionID DeckSessionID) error {
	return b.runner.SelectWindow(string(sessionID))
}

// Close is a no-op for tmuxBackend.  The tmux session is intentionally persistent
// so Claude Code continues running after claude-deck exits.
func (b *tmuxBackend) Close() {}

// EnsurePreview creates the __preview__ tmux window if it does not exist.
// The window runs "claude-deck --preview" — used in Ghostty split mode to
// display JSONL logs for the session selected in the list TUI.
func (b *tmuxBackend) EnsurePreview() error {
	if b.runner.HasWindow(previewWindowName) {
		return nil
	}
	// Use the full path of the running binary so the preview window can be
	// launched even when claude-deck is not installed globally in PATH.
	execPath, err := os.Executable()
	if err != nil {
		execPath = "claude-deck"
	}
	execPath, _ = filepath.EvalSymlinks(execPath)
	opts := tmux.WindowOpts{
		Command: execPath + " --preview",
	}
	if err := b.runner.NewWindow(previewWindowName, opts); err != nil {
		return fmt.Errorf("create preview window: %w", err)
	}
	debuglog.Printf("[tmuxBackend.EnsurePreview] created %s window: %s", previewWindowName, opts.Command)
	return nil
}

// FocusPreview switches the tmux client to the __preview__ window.
// Called when the user selects a non-running session in split mode.
func (b *tmuxBackend) FocusPreview() error {
	return b.runner.SelectWindow(previewWindowName)
}

// KillPreview kills the __preview__ tmux window.
// Called on claude-deck exit in split mode to clean up the preview subprocess.
func (b *tmuxBackend) KillPreview() error {
	if !b.runner.HasWindow(previewWindowName) {
		return nil
	}
	return b.runner.KillWindow(previewWindowName)
}

// Reconcile synchronises tmux window state against the provided session list.
// For each session with a live tmux window, an exit-watcher goroutine is
// re-attached and the session ID is included in ReconcileResult.LivePIDs.
// Orphaned tmux windows (no corresponding deck session) are killed.
func (b *tmuxBackend) Reconcile(sessions []*Session) (ReconcileResult, error) {
	windows, err := b.runner.ListWindows()
	if err != nil {
		debuglog.Printf("[tmuxBackend.Reconcile] ListWindows failed: %v", err)
		return ReconcileResult{}, err
	}

	// Build lookup maps.
	liveWindows := make(map[string]int, len(windows)) // windowName → panePID
	for _, w := range windows {
		liveWindows[w.Name] = w.PanePID
	}
	deckIDs := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		deckIDs[string(s.ID)] = true
	}

	// Re-attach exit watchers for live sessions and collect their PIDs.
	livePIDs := make(map[DeckSessionID]int)
	for _, sess := range sessions {
		windowName := string(sess.ID)
		if panePID, alive := liveWindows[windowName]; alive {
			debuglog.Printf("[tmuxBackend.Reconcile] re-attaching exit watcher session=%s pid=%d", sess.ID, panePID)
			livePIDs[sess.ID] = panePID
			channel := tmux.ExitChannel(windowName)
			wn := windowName
			rctx := b.ctx
			go func(s *Session, ch, wname string) {
				// Race guard: check HasWindow before blocking on WaitFor.
				// If the pane already exited while we were building the livePIDs map,
				// the pane-exited hook may have fired the channel signal before WaitFor
				// starts, causing WaitFor to block forever. Fire onExit immediately instead.
				if !b.runner.HasWindow(wname) {
					debuglog.Printf("[tmuxBackend.Reconcile] pane already gone session=%s", s.ID)
				} else if err := b.runner.WaitForCtx(rctx, ch); err != nil {
					if rctx.Err() != nil {
						return
					}
				}
				if b.onExit != nil {
					b.onExit(s)
				}
			}(sess, channel, wn)
		}
	}

	// Kill orphaned tmux windows (window exists but no deck session owns it).
	// previewWindowName is managed by EnsurePreview, not by deck sessions — skip it.
	for _, w := range windows {
		if w.Name == previewWindowName || deckIDs[w.Name] {
			continue
		}
		debuglog.Printf("[tmuxBackend.Reconcile] killing orphaned window=%s", w.Name)
		_ = b.runner.KillWindow(w.Name)
	}

	// Killing the last window destroys the tmux session itself.
	// Re-create it so subsequent NewWindow calls have a live session to target.
	if !b.runner.HasSession() {
		debuglog.Printf("[tmuxBackend.Reconcile] session died after orphan cleanup, re-creating")
		if err := b.runner.NewSession(); err != nil {
			debuglog.Printf("[tmuxBackend.Reconcile] re-creating session failed: %v", err)
		} else {
			b.runner.ApplyDefaultOptions()
		}
	}

	return ReconcileResult{LivePIDs: livePIDs}, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// buildShellCommand assembles a shell-safe command string from a binary path
// and argument list.  Each component is single-quote escaped to preserve
// spaces, special characters, and prompt text in -p arguments.
func buildShellCommand(cmd string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, shellescape(cmd))
	for _, a := range args {
		parts = append(parts, shellescape(a))
	}
	return strings.Join(parts, " ")
}

// shellescape wraps s in POSIX single quotes, replacing embedded single quotes
// with the canonical '"'"' escape sequence.
func shellescape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
