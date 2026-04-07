package session

// Default PTY dimensions (must match pty.StartOptions defaults).
const (
	defaultPTYCols = 120
	defaultPTYRows = 40
)

// GetPTYDisplayLines returns the current screen state from the virtual terminal.
// Non-blocking. Returns nil if no PTYDisplay is attached (HostExternal).
func (s *Session) GetPTYDisplayLines() []string {
	if s.display != nil {
		return s.display.Lines()
	}
	return nil
}

// GetPTYCursorPosition returns the cursor's position within GetPTYDisplayLines().
// X is the terminal column (0-indexed), Y is the line index.
// Returns (0, 0) if no PTYDisplay is attached.
func (s *Session) GetPTYCursorPosition() (x, y int) {
	if s.display != nil {
		return s.display.CursorPosition()
	}
	return 0, 0
}
