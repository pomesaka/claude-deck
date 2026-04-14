package session

// Default PTY dimensions (must match pty.StartOptions defaults).
const (
	defaultPTYCols = 120
	defaultPTYRows = 40
)

// GetPTYDisplayLines returns the current screen state from the virtual terminal.
// Non-blocking. Returns nil if no embedded PTYDisplay is active.
func (s *Session) GetPTYDisplayLines() []string {
	if rp := s.process.Load(); rp != nil && rp.display != nil {
		return rp.display.Lines()
	}
	return nil
}

// GetDisplayVersion returns the PTY display version counter.
// Non-blocking. Returns 0 if no embedded PTYDisplay is active.
// The TUI uses this to skip redundant SetContentLines calls.
func (s *Session) GetDisplayVersion() uint64 {
	if rp := s.process.Load(); rp != nil && rp.display != nil {
		return rp.display.Version()
	}
	return 0
}

// GetPTYCursorPosition returns the cursor's position within GetPTYDisplayLines().
// X is the terminal column (0-indexed), Y is the line index.
// Returns (0, 0) if no embedded PTYDisplay is active.
func (s *Session) GetPTYCursorPosition() (x, y int) {
	if rp := s.process.Load(); rp != nil && rp.display != nil {
		return rp.display.CursorPosition()
	}
	return 0, 0
}
