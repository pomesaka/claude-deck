package agentruntime

// ClaudeRuntime adapts claude-deck's runtime-neutral lifecycle operations to
// Claude Code CLI flags.
type ClaudeRuntime struct {
	Command string
}

func (r ClaudeRuntime) Provider() Provider {
	return ProviderClaude
}

func (r ClaudeRuntime) StartSpec(req StartRequest) StartSpec {
	args := buildClaudeStartArgs(req)
	return StartSpec{
		Command: r.Command,
		Args:    args,
	}
}

// buildClaudeStartArgs assembles CLI args for `claude` from semantic launch
// parameters. Keeping this in the Claude adapter prevents Codex-specific modes
// from leaking into Manager.
//
// The launch modes map to:
//   - resume + fork → --resume <id> --fork-session
//   - resume        → --resume <id>
//   - new + prompt  → -p <prompt>
//   - new           → no conversation args
func buildClaudeStartArgs(req StartRequest) []string {
	var args []string
	switch req.Mode {
	case LaunchResume:
		if req.SessionID != "" {
			args = append(args, "--resume", req.SessionID)
		}
	case LaunchFork:
		if req.SessionID != "" {
			args = append(args, "--resume", req.SessionID, "--fork-session")
		}
	case LaunchNew:
		if req.Prompt != "" {
			args = append(args, "-p", req.Prompt)
		}
	}
	if req.PermissionMode != "" {
		args = append(args, "--permission-mode", req.PermissionMode)
	}
	if req.SessionName != "" {
		args = append(args, "--agent", req.SessionName)
	}
	return append(args, req.AdditionalArgs...)
}
