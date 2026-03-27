// Package event defines the typed event vocabulary for claude-deck's internal
// event bus. All state-changing signals that flow between subsystems
// (PTY, hook watcher, JSONL watcher, process monitor) are represented here.
//
// Design intent: events are produced by edge subsystems and consumed by the
// session Manager. Using explicit types instead of ad-hoc function calls
// makes state transitions easy to enumerate and test.
package event

// Kind identifies the category of an event.
type Kind int

const (
	// ProcessStarted fires when a PTY process begins for a session.
	ProcessStarted Kind = iota
	// ProcessExited fires when a PTY process terminates.
	ProcessExited
	// ProcessOutput fires for each chunk of raw PTY output.
	// High frequency — not routed through the coordinator by default.
	ProcessOutput
	// HookNotification fires when Claude Code emits a notification hook
	// (permission prompt, elicitation dialog, idle prompt, stop).
	HookNotification
	// SessionRotated fires when /clear or compact creates a new Claude session ID.
	// The old ID stays in SessionChain; the new ID is appended.
	SessionRotated
	// JSONLUpdated fires when a JSONL file is written (LastActivity update).
	JSONLUpdated
	// JSONLDiscovered fires when a new JSONL file appears (external session import).
	JSONLDiscovered
)

// Event carries a typed signal from a producer to the session coordinator.
//
// SessionID is the claude-deck session ID (Session.ID), not the Claude Code
// session ID. It is empty for events that are not yet associated with a session
// (e.g. JSONLDiscovered before the session is imported).
type Event struct {
	Kind      Kind
	SessionID string // deck session ID (Session.ID), may be empty
	Data      any    // payload; nil when no additional data is needed
}

// ProcessStartedData is the payload for ProcessStarted events.
type ProcessStartedData struct {
	PID int
}

// ProcessExitedData is the payload for ProcessExited events.
type ProcessExitedData struct {
	ExitCode int
}

// HookNotificationData is the payload for HookNotification events.
type HookNotificationData struct {
	// ClaudeSessionID is the Claude Code session ID from the hook event.
	ClaudeSessionID  string
	NotificationType string // "permission_prompt", "elicitation_dialog", "idle_prompt"
}

// SessionRotatedData is the payload for SessionRotated events.
type SessionRotatedData struct {
	OldClaudeID string
	NewClaudeID string
	// Source is "clear" or "compact".
	Source string
}

// JSONLUpdatedData is the payload for JSONLUpdated events.
type JSONLUpdatedData struct {
	ClaudeSessionID string
}

// JSONLDiscoveredData is the payload for JSONLDiscovered events.
type JSONLDiscoveredData struct {
	ClaudeSessionID string
}
