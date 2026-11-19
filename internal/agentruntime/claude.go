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
//   - new           → no conversation args
//   - a prompt      → "-- <prompt>" at the very end, in any mode. The session stays
//     interactive with the prompt as its first message.
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
	}
	if req.PermissionMode != "" {
		args = append(args, "--permission-mode", req.PermissionMode)
	}
	if req.SessionName != "" {
		args = append(args, "--name", req.SessionName)
	}
	if req.PluginDir != "" {
		args = append(args, "--plugin-dir", req.PluginDir)
	}
	args = append(args, req.AdditionalArgs...)
	if req.Prompt != "" {
		// WHY "--" を挟んで最後に置く: --add-dir のように値を複数取るオプションの後ろに置くと
		// その値として読まれ、"-" で始まる指示はオプションと読まれる（Claude Code 2.1.327 で確認）。
		// 長い指示は tmux のコマンドの長さの上限に当たるので、tmuxBackend がスクリプトを通して渡す。
		args = append(args, "--", req.Prompt)
	}
	return args
}
