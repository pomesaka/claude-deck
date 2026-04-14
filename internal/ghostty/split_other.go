//go:build !darwin

package ghostty

import "errors"

// IsRunningInGhostty always returns false on non-macOS platforms.
// Ghostty AppleScript IPC is macOS-only.
func IsRunningInGhostty() bool {
	return false
}

// SplitRight is not supported on non-macOS platforms.
func SplitRight(_ string) (string, error) {
	return "", errors.New("ghostty split: macOS only")
}

// CloseTerminal is not supported on non-macOS platforms.
func CloseTerminal(_ string) error {
	return errors.New("ghostty close terminal: macOS only")
}

// ResizeSplit is not supported on non-macOS platforms.
func ResizeSplit(_ int) error {
	return errors.New("ghostty resize split: macOS only")
}

// FocusLeft is not supported on non-macOS platforms.
func FocusLeft() error {
	return errors.New("ghostty focus left: macOS only")
}

// FocusRight is not supported on non-macOS platforms.
func FocusRight() error {
	return errors.New("ghostty focus right: macOS only")
}
