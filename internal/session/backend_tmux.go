package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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
// The tmux session is persistent: it survives claude-deck restarts and users
// can detach/reattach freely via `tmux attach -t <session>`.
type tmuxBackend struct {
	runner *tmux.Runner
}

// newTmuxBackend creates a tmuxBackend using the given runner.
func newTmuxBackend(runner *tmux.Runner) *tmuxBackend {
	return &tmuxBackend{runner: runner}
}

// StartProcess creates a tmux window for the session and starts Claude Code in it.
//
// The window runs "<claude ...>; <OnExit ...>" through the shell, so OnExit runs in
// the same pane after Claude Code exits.
// WHY tmux の hook を使わない: ウィンドウやペインに付けた pane-exited hook は、
// tmux 3.6a ではペインが自然に終了しても発火しなかった（2026-10-09、専用ソケット tmux -L で確認）。
// WHY NOT グローバル（-g）の pane-exited: tmux サーバーのすべてのペインに効くので、
// claude-deck 以外のペインでも動く。発火するかは確かめていない。
func (b *tmuxBackend) StartProcess(sessionID DeckSessionID, opts ProcessStartOpts) (int, error) {
	windowName := string(sessionID)

	// Build a shell-safe command string from the binary and pre-assembled args.
	shellCmd := buildShellCommand(opts.Command, opts.Args)
	if len(opts.OnExit) > 0 {
		shellCmd += "; " + buildShellCommand(opts.OnExit[0], opts.OnExit[1:])
	}
	debuglog.Printf("[tmuxBackend] StartProcess session=%s cmd=%s", sessionID, shellCmd)
	shellCmd, err := launchCommand(shellCmd)
	if err != nil {
		return 0, err
	}

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
			return 0, fmt.Errorf("tmux new-window: %w (session re-create also failed: %v)", err, rerr)
		}
		b.runner.ApplyDefaultOptions()
		if rerr := b.runner.NewWindow(windowName, wopts); rerr != nil {
			return 0, fmt.Errorf("tmux new-window: %w", rerr)
		}
	}

	pid, err := b.runner.PanePID(windowName)
	if err != nil {
		// The window may already be gone if the command failed immediately.
		// OnExit has recorded the exit in that case, so this is not an error.
		debuglog.Printf("[tmuxBackend] PanePID failed session=%s: %v", sessionID, err)
	}
	return pid, nil
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

// LiveSessions returns every session window in the tmux session with its pane PID.
// The preview window is not a session and is excluded.
func (b *tmuxBackend) LiveSessions() (map[DeckSessionID]int, error) {
	windows, err := b.runner.ListWindows()
	if err != nil {
		// tmux セッションごと消えていれば、どのセッションのウィンドウも無い。
		// エラーのまま返すと、呼び出し側は終了を記録できない。
		if !b.runner.HasSession() {
			return map[DeckSessionID]int{}, nil
		}
		return nil, err
	}
	live := make(map[DeckSessionID]int, len(windows))
	for _, w := range windows {
		if w.Name == previewWindowName {
			continue
		}
		live[DeckSessionID(w.Name)] = w.PanePID
	}
	return live, nil
}

// KillWindows kills the given sessions' tmux windows.
func (b *tmuxBackend) KillWindows(ids []DeckSessionID) error {
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		debuglog.Printf("[tmuxBackend.KillWindows] killing window=%s", id)
		_ = b.runner.KillWindow(string(id))
	}

	// Killing the last window destroys the tmux session itself.
	// Re-create it so subsequent NewWindow calls have a live session to target.
	if !b.runner.HasSession() {
		debuglog.Printf("[tmuxBackend.KillWindows] session died after killing windows, re-creating")
		if err := b.runner.NewSession(); err != nil {
			return fmt.Errorf("re-creating tmux session: %w", err)
		}
		b.runner.ApplyDefaultOptions()
	}
	return nil
}

// Focus selects the session's window in the tmux session, making it visible
// in any attached tmux client.  This is called on session list navigation for
// near-instant (~0ms) switching between Claude Code sessions.
func (b *tmuxBackend) Focus(sessionID DeckSessionID) error {
	return b.runner.SelectWindow(string(sessionID))
}

// EnsurePreview creates the __preview__ tmux window if it does not exist.
// The window runs "claude-deck --preview" — used in Ghostty split mode to
// display JSONL logs for the session selected in the list TUI.
func (b *tmuxBackend) EnsurePreview() error {
	if b.runner.HasWindow(previewWindowName) {
		return nil
	}
	opts := tmux.WindowOpts{
		Command: previewCommand(),
	}
	if err := b.runner.NewWindow(previewWindowName, opts); err != nil {
		return fmt.Errorf("create preview window: %w", err)
	}
	debuglog.Printf("[tmuxBackend.EnsurePreview] created %s window: %s", previewWindowName, opts.Command)
	return nil
}

// RestartPreview runs the preview window's process again from the running
// binary, or creates the window when there is none. Used after a reload, so
// the preview runs the new build too.
// WHY respawn-window: ウィンドウを消して作り直すと、右ペインの tmux のクライアントが別のウィンドウへ移る。
func (b *tmuxBackend) RestartPreview() error {
	if !b.runner.HasWindow(previewWindowName) {
		return b.EnsurePreview()
	}
	return b.runner.RespawnWindow(previewWindowName, previewCommand())
}

// previewCommand runs the preview from the full path of the running binary,
// so the window starts even when claude-deck is not on PATH.
func previewCommand() string {
	execPath, err := os.Executable()
	if err != nil {
		execPath = "claude-deck"
	}
	execPath, _ = filepath.EvalSymlinks(execPath)
	return execPath + " --preview"
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

// ── Helpers ──────────────────────────────────────────────────────────────────

// maxInlineCommand is the longest command passed to tmux new-window as it is.
// WHY: tmux はコマンドの長さに上限があり、約 16KB の new-window を "Command too long" で
// 断った（tmux 3.6a、2026-11 に確認）。長い最初の指示（new --prompt -）で届く。余裕を取る。
const maxInlineCommand = 4096

// launchCommand returns shellCmd itself when it is short enough for tmux, and
// otherwise writes it to a temporary script and returns a command running that
// script. The script removes itself when it starts.
func launchCommand(shellCmd string) (string, error) {
	if len(shellCmd) <= maxInlineCommand {
		return shellCmd, nil
	}
	f, err := os.CreateTemp("", "claude-deck-launch-*.sh")
	if err != nil {
		return "", fmt.Errorf("writing the launch script: %w", err)
	}
	script := "#!/bin/sh\nrm -f \"$0\"\n" + shellCmd + "\n"
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("writing the launch script: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("writing the launch script: %w", err)
	}
	return buildShellCommand("/bin/sh", []string{f.Name()}), nil
}

// buildShellCommand assembles a shell-safe command string from a binary path
// and argument list.  Each component is single-quote escaped to preserve
// spaces, newlines, special characters, and the first prompt as one argument each.
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

// Screen returns what the session's window shows, with its colours.
func (b *tmuxBackend) Screen(sessionID DeckSessionID) (string, error) {
	return b.runner.CapturePane(string(sessionID))
}

// TypeLine types text into the session's window and presses Enter.
func (b *tmuxBackend) TypeLine(sessionID DeckSessionID, text string) error {
	return b.runner.SendLine(string(sessionID), text)
}
