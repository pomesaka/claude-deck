package usage

import "time"

// format is what differs between the transcripts of the agent runtimes: where
// the files are, how a session ID is read from a path, and how a line is read.
// Reader, MultiWatcher and LogStreamer hold one and never ask which runtime it is.
type format interface {
	// files returns every transcript under baseDir.
	files(baseDir string) []string
	// sessionID returns the runtime session ID of the transcript at path.
	sessionID(path string) string
	// userMessageMarker is a byte sequence that only a line holding a user message contains.
	userMessageMarker() []byte
	// quickInfo reads the head of the transcript; LastActivity is mtime.
	quickInfo(path string, mtime time.Time) *SessionInfo
	// info reads the whole transcript.
	info(path string) *SessionInfo
	// tokens reads the token usage of the transcript at path.
	tokens(path, sessionID string) *TokenStats
	// runtimeActivity reads what the runtime is doing from the tail of the transcript.
	runtimeActivity(path string) RuntimeActivity
	// logLine appends the log entries of one line to s and reports whether s changed.
	logLine(s *LogStreamer, line []byte) bool
}

// formatFor returns the format of a runtime provider (config.toml runtime.provider).
// Anything but "codex" is Claude Code, as in config.RuntimeProvider.
func formatFor(provider string) format {
	if provider == "codex" {
		return codexFormat{}
	}
	return claudeFormat{}
}
