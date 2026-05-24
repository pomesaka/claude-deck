package agentruntime

// CodexRuntime adapts runtime-neutral lifecycle operations to the Codex CLI.
type CodexRuntime struct {
	Command string
}

func (r CodexRuntime) Provider() Provider {
	return ProviderCodex
}

func (r CodexRuntime) StartSpec(req StartRequest) StartSpec {
	return StartSpec{
		Command: r.Command,
		Args:    buildCodexStartArgs(req),
	}
}

func buildCodexStartArgs(req StartRequest) []string {
	var args []string
	if req.WorkDir != "" {
		args = append(args, "--cd", req.WorkDir)
	}

	// Claude's "default" permission mode has no Codex equivalent. Omitting the
	// flag keeps Codex on its configured/default approval policy.
	if req.PermissionMode != "" && req.PermissionMode != "default" {
		args = append(args, "--ask-for-approval", req.PermissionMode)
	}
	if hasCodexAddDir(req.AdditionalArgs) && !hasCodexSandbox(req.AdditionalArgs) {
		args = append(args, "--sandbox", "workspace-write")
	}
	args = append(args, req.AdditionalArgs...)

	switch req.Mode {
	case LaunchResume:
		args = append(args, "resume")
		if req.SessionID != "" {
			args = append(args, req.SessionID)
		}
	case LaunchFork:
		args = append(args, "fork")
		if req.SessionID != "" {
			args = append(args, req.SessionID)
		}
	}
	if req.Prompt != "" {
		args = append(args, req.Prompt)
	}
	return args
}

func hasCodexAddDir(args []string) bool {
	for _, arg := range args {
		if arg == "--add-dir" {
			return true
		}
	}
	return false
}

func hasCodexSandbox(args []string) bool {
	for _, arg := range args {
		if arg == "--sandbox" || arg == "-s" {
			return true
		}
	}
	return false
}
