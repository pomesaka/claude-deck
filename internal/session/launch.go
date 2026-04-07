package session

// LaunchKind describes how a session is being started.
type LaunchKind int

const (
	// LaunchNew creates a fresh session with a new Claude Code process.
	LaunchNew LaunchKind = iota
	// LaunchResume restarts a completed session using --resume.
	LaunchResume
	// LaunchFork creates a new session that forks an existing conversation.
	LaunchFork
	// LaunchExternal creates a session whose PTY is hosted by an external
	// terminal (e.g. Ghostty / tmux). claude-deck manages metadata only —
	// no emulator, no PTY output capture.
	LaunchExternal
)

func (k LaunchKind) String() string {
	switch k {
	case LaunchNew:
		return "New"
	case LaunchResume:
		return "Resume"
	case LaunchFork:
		return "Fork"
	case LaunchExternal:
		return "External"
	default:
		return "Unknown"
	}
}

// LaunchIntent captures the user's intention when starting a session.
// Instead of three separate methods with overlapping logic (CreateSession,
// ResumeSession, ForkSession), callers construct a LaunchIntent and pass it
// to Manager.Launch. The Manager uses Kind to dispatch to the appropriate
// internal workflow while sharing common setup (PTY start, watchProcess,
// persist, notify).
type LaunchIntent struct {
	Kind LaunchKind

	// RepoPath is the repository root (.jj parent). Required for New and Fork.
	RepoPath string
	// WorkingDir is the directory to run claude in (may differ from RepoPath for sub-projects).
	// Required for New, optional for Resume (falls back to session's stored path).
	WorkingDir string
	// WithWorkspace controls whether a jj workspace is created. Only used for New.
	WithWorkspace bool

	// SessionID is the deck session ID to resume. Required for Resume and Fork.
	SessionID DeckSessionID

	// Cols and Rows are the terminal dimensions for the PTY.
	Cols int
	Rows int
}
