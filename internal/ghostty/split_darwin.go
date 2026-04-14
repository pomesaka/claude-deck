//go:build darwin

package ghostty

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// IsRunningInGhostty reports whether the current process is running inside
// a Ghostty terminal.  Ghostty sets TERM_PROGRAM=ghostty.
func IsRunningInGhostty() bool {
	return os.Getenv("TERM_PROGRAM") == "ghostty"
}

// SplitRight splits the focused terminal in the front Ghostty window to the
// right, types command (followed by Enter) into the new pane, and returns
// the UUID of the newly created terminal.
//
// We use "initial input" rather than "command" because Ghostty passes the
// command property through `exec -l <command>` which breaks multi-word
// commands.  "initial input" types the text into the default shell instead,
// correctly handling arguments and PATH resolution.
//
// The returned UUID can be used later with CloseTerminal to close the pane.
//
// Note: This requires macOS and Ghostty ≥ 1.3 (AppleScript preview API).
func SplitRight(command string) (string, error) {
	// Append return so the command executes automatically.
	input := command + "\n"
	script := fmt.Sprintf(`
tell application "Ghostty"
    set cfg to new surface configuration
    set initial input of cfg to %s
    tell front window
        set newTerm to split focused terminal of selected tab direction right with configuration cfg
        return id of newTerm
    end tell
end tell`, applescriptString(input))

	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ghostty split right: %w (osascript: %s)", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// CloseTerminal closes the Ghostty terminal identified by uuid.
// uuid is the value returned by SplitRight.
func CloseTerminal(uuid string) error {
	if uuid == "" {
		return nil
	}
	script := fmt.Sprintf(`
tell application "Ghostty"
    close terminal id %s
end tell`, applescriptString(uuid))

	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ghostty close terminal: %w (osascript: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ResizeSplit resizes the left split pane to the given pixel width.
// Uses Ghostty's resize_split action on the focused terminal.
//
// This must be called after SplitRight with a small delay (0.25s+) so
// that the new split exists before the resize action fires.
func ResizeSplit(pixels int) error {
	script := fmt.Sprintf(`
tell application "Ghostty"
    tell front window
        perform action "resize_split:left,%d" on focused terminal of selected tab
    end tell
end tell`, pixels)

	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ghostty resize split: %w (osascript: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// FocusRight moves keyboard focus to the right split pane.
// Call this when the user wants to interact with the tmux client in the right pane.
// Uses goto_split:next which reliably updates Ghostty's active-pane visual indicator.
func FocusRight() error {
	script := `
tell application "Ghostty"
    tell front window
        perform action "goto_split:next" on focused terminal of selected tab
    end tell
end tell`
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ghostty focus right: %w (osascript: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// FocusLeft moves keyboard focus to the left split pane.
// After SplitRight, the newly created right pane is focused; call this to
// return focus to the claude-deck TUI in the left pane.
func FocusLeft() error {
	script := `
tell application "Ghostty"
    tell front window
        perform action "goto_split:left" on focused terminal of selected tab
    end tell
end tell`
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ghostty focus left: %w (osascript: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// applescriptString wraps s in an AppleScript string literal, escaping
// backslashes and double-quotes.
//
// Note: other control characters (e.g. newline, tab) are NOT escaped — they
// are passed as-is inside the AppleScript string literal.  AppleScript allows
// literal newlines in string values, which is intentional here: SplitRight
// appends "\n" so that the command executes automatically via "initial input".
func applescriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
