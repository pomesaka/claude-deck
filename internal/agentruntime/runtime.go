// Package agentruntime describes the external coding-agent CLI that backs a
// deck session.
package agentruntime

// Provider identifies the backing agent runtime implementation.
type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
)

// LaunchMode describes how the runtime process should start.
type LaunchMode int

const (
	LaunchNew LaunchMode = iota
	LaunchResume
	LaunchFork
)

// StartRequest is a runtime-neutral process start description.
// Runtime implementations translate it to concrete CLI command/args.
type StartRequest struct {
	Mode LaunchMode

	// SessionID is the runtime conversation/thread ID used by resume/fork.
	SessionID string
	Prompt    string
	WorkDir   string

	SessionName    string
	PermissionMode string
	AdditionalArgs []string
}

// StartSpec contains the concrete process command and arguments for a runtime.
type StartSpec struct {
	Command string
	Args    []string
}

// Runtime translates deck lifecycle operations into runtime-specific CLI
// commands and arguments.
type Runtime interface {
	Provider() Provider
	StartSpec(StartRequest) StartSpec
}
