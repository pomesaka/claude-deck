// Package tmux provides a thin typed wrapper around the tmux CLI.
// All operations target a single named tmux session managed by claude-deck.
// The zero value of Runner is usable with defaults ("tmux" / "claude-deck").
package tmux

import (
	"bufio"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

const (
	// DefaultCommand is the tmux executable name.
	DefaultCommand = "tmux"
	// DefaultSessionName is the tmux session owned by claude-deck.
	DefaultSessionName = "claude-deck"
)

// Runner wraps the tmux CLI.  Every public method issues exactly one
// `exec.Command("tmux", ...)` call — no state, no goroutines, no retries.
// Callers are responsible for error handling and process synchronisation.
type Runner struct {
	Command     string // tmux binary path; defaults to DefaultCommand
	SessionName string // tmux session name; defaults to DefaultSessionName
}

// cmd returns the resolved binary name.
func (r *Runner) cmd() string {
	if r.Command == "" {
		return DefaultCommand
	}
	return r.Command
}

// sess returns the resolved session name.
func (r *Runner) sess() string {
	if r.SessionName == "" {
		return DefaultSessionName
	}
	return r.SessionName
}

// run executes a tmux sub-command and returns trimmed combined output.
func (r *Runner) run(args ...string) (string, error) {
	out, err := exec.Command(r.cmd(), args...).CombinedOutput()
	return strings.TrimRight(string(out), "\n"), err
}

// ── Session lifecycle ────────────────────────────────────────────────────────

// HasSession returns true if the named tmux session exists.
func (r *Runner) HasSession() bool {
	_, err := r.run("has-session", "-t", r.sess())
	return err == nil
}

// NewSession creates a new detached tmux session.
func (r *Runner) NewSession() error {
	_, err := r.run("new-session", "-d", "-s", r.sess())
	return err
}

// KillSession destroys the session and all its windows.
func (r *Runner) KillSession() error {
	_, err := r.run("kill-session", "-t", r.sess())
	return err
}

// SetOption sets a session-scoped tmux option (e.g., "status" → "off").
// Use this after NewSession to configure the session appearance.
func (r *Runner) SetOption(option, value string) error {
	_, err := r.run("set-option", "-t", r.sess(), option, value)
	return err
}

// ── Window lifecycle ─────────────────────────────────────────────────────────

// WindowOpts configures the window created by NewWindow.
type WindowOpts struct {
	Command string   // shell command to run in the new window
	WorkDir string   // start directory passed to tmux -c
	Env     []string // KEY=VALUE pairs injected with -e (requires tmux ≥ 3.0)
}

// NewWindow creates a named window in the session.
// The window runs opts.Command from opts.WorkDir with opts.Env injected.
func (r *Runner) NewWindow(windowName string, opts WindowOpts) error {
	_, err := r.run(newWindowArgs(r.sess(), windowName, opts)...)
	return err
}

// KillWindow closes the named window and all its panes.
func (r *Runner) KillWindow(windowName string) error {
	_, err := r.run("kill-window", "-t", r.sess()+":"+windowName)
	return err
}

// HasWindow returns true if a window named windowName exists in the session.
func (r *Runner) HasWindow(windowName string) bool {
	out, err := r.run("list-windows", "-t", r.sess(), "-F", "#{window_name}")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == windowName {
			return true
		}
	}
	return false
}

// WindowInfo holds metadata about a tmux window returned by ListWindows.
type WindowInfo struct {
	Name    string
	PanePID int // PID of the window's first pane process (#{pane_pid})
}

// ListWindows returns metadata for all windows in the session.
func (r *Runner) ListWindows() ([]WindowInfo, error) {
	out, err := r.run("list-windows", "-t", r.sess(), "-F", "#{window_name} #{pane_pid}")
	if err != nil {
		return nil, err
	}
	return parseWindowList(out), nil
}

// SelectWindow makes windowName the active (visible) window.
// This is the primary mechanism for ~0ms session switching in the tmux client.
func (r *Runner) SelectWindow(windowName string) error {
	_, err := r.run("select-window", "-t", r.sess()+":"+windowName)
	return err
}

// PanePID returns the OS PID of the first pane in the named window.
// Used as a fallback kill target when KillWindow fails.
func (r *Runner) PanePID(windowName string) (int, error) {
	out, err := r.run("list-panes", "-t", r.sess()+":"+windowName, "-F", "#{pane_pid}")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// ── I/O ─────────────────────────────────────────────────────────────────────

// SendKeys sends keystrokes to the named window's active pane.
// This is used by WriteInput in tmux mode; for typical workflows the user
// interacts directly via the tmux client and this method is rarely called.
func (r *Runner) SendKeys(windowName, keys string) error {
	_, err := r.run("send-keys", "-t", r.sess()+":"+windowName, keys)
	return err
}

// ── Exit detection ───────────────────────────────────────────────────────────

// SetHook sets a tmux hook on a specific window.
// hookName is the tmux hook event (e.g., "pane-exited").
// hookCmd is the tmux command string to execute when the hook fires.
//
// Exit-detection pattern:
//
//	SetHook(name, "pane-exited", "wait-for -S "+ExitChannel(name))
//	go WaitFor(ExitChannel(name))  // blocks until pane exits
func (r *Runner) SetHook(windowName, hookName, hookCmd string) error {
	target := r.sess() + ":" + windowName
	_, err := r.run("set-hook", "-t", target, hookName, hookCmd)
	return err
}

// WaitFor blocks the calling goroutine until WaitForSignal is called with the
// same channel name.  This is the blocking half of the exit-detection pair.
func (r *Runner) WaitFor(channel string) error {
	_, err := r.run("wait-for", channel)
	return err
}

// WaitForSignal unblocks any WaitFor goroutine waiting on channel.
// Typically called from the pane-exited hook set up by SetHook.
func (r *Runner) WaitForSignal(channel string) error {
	_, err := r.run("wait-for", "-S", channel)
	return err
}

// ExitChannel returns the wait-for channel name for the given window's exit event.
// Deterministic and unique per window name.
func ExitChannel(windowName string) string {
	return fmt.Sprintf("deck-exit-%s", windowName)
}

// ── Unexported helpers (argument construction, tested via package-level tests) ─

// newWindowArgs builds the tmux new-window argument list.
// Separated from NewWindow so the argument construction can be unit-tested
// without spawning real tmux processes.
func newWindowArgs(sessionName, windowName string, opts WindowOpts) []string {
	args := []string{"new-window", "-t", sessionName + ":", "-n", windowName}
	if opts.WorkDir != "" {
		args = append(args, "-c", opts.WorkDir)
	}
	for _, e := range opts.Env {
		args = append(args, "-e", e)
	}
	if opts.Command != "" {
		args = append(args, opts.Command)
	}
	return args
}

// parseWindowList parses the output of `list-windows -F "#{window_name} #{pane_pid}"`.
func parseWindowList(out string) []WindowInfo {
	var windows []WindowInfo
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		pid, _ := strconv.Atoi(parts[1])
		windows = append(windows, WindowInfo{Name: parts[0], PanePID: pid})
	}
	return windows
}
