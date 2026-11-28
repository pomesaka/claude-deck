package main

import (
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/pomesaka/claude-deck/internal/session"
)

func TestParseCLIArgs(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_DECK_SESSION_ID", "")

	tests := []struct {
		name    string
		command string
		args    []string
		stdin   io.Reader // nil: reading stdin fails the test
		want    cliRequest
	}{
		{
			name:    "new defaults to working directory with workspace",
			command: "new",
			want:    cliRequest{Op: "new", Dir: wd},
		},
		{
			name:    "new with dir and no workspace",
			command: "new",
			args:    []string{"--dir", "/tmp/repo", "--no-workspace"},
			want:    cliRequest{Op: "new", Dir: "/tmp/repo", NoWorkspace: true},
		},
		{
			name:    "new with a prompt",
			command: "new",
			args:    []string{"--prompt", "fix the bug"},
			want:    cliRequest{Op: "new", Dir: wd, Prompt: "fix the bug"},
		},
		{
			name:    "new with a prompt that starts with dashes",
			command: "new",
			args:    []string{"--prompt", "--help me"},
			want:    cliRequest{Op: "new", Dir: wd, Prompt: "--help me"},
		},
		{
			name:    "new with dir, no workspace and a prompt",
			command: "new",
			args:    []string{"--dir", "/tmp/repo", "--no-workspace", "--prompt", "hi"},
			want:    cliRequest{Op: "new", Dir: "/tmp/repo", NoWorkspace: true, Prompt: "hi"},
		},
		{
			name:    "new reads the prompt from stdin, keeping inner newlines",
			command: "new",
			args:    []string{"--prompt", "-"},
			stdin:   strings.NewReader("line1\nline2\n"),
			want:    cliRequest{Op: "new", Dir: wd, Prompt: "line1\nline2"},
		},
		{
			name:    "fork another session",
			command: "fork",
			args:    []string{"--session", "anna-8cc7"},
			want:    cliRequest{Op: "fork", Target: "anna-8cc7"},
		},
		{
			name:    "fork with a prompt",
			command: "fork",
			args:    []string{"--session", "anna-8cc7", "--prompt", "fix"},
			want:    cliRequest{Op: "fork", Target: "anna-8cc7", Prompt: "fix"},
		},
		{
			name:    "fork reads the prompt from stdin, keeping inner newlines",
			command: "fork",
			args:    []string{"--session", "anna-8cc7", "--prompt", "-"},
			stdin:   strings.NewReader("line1\nline2\n"),
			want:    cliRequest{Op: "fork", Target: "anna-8cc7", Prompt: "line1\nline2"},
		},
		{
			name:    "list",
			command: "list",
			want:    cliRequest{Op: "list"},
		},
		{
			name:    "list filtered by alias",
			command: "list",
			args:    []string{"--alias", "review"},
			want:    cliRequest{Op: "list", Filter: sessionFilter{Alias: "review"}},
		},
		{
			name:    "list filtered by repository",
			command: "list",
			args:    []string{"--repo", "deck"},
			want:    cliRequest{Op: "list", Filter: sessionFilter{Repo: "deck"}},
		},
		{
			name:    "list filtered by alias and repository",
			command: "list",
			args:    []string{"--alias", "review", "--repo", "deck"},
			want:    cliRequest{Op: "list", Filter: sessionFilter{Alias: "review", Repo: "deck"}},
		},
		{
			name:    "alias of another session",
			command: "alias",
			args:    []string{"--session", "anna-8cc7", "review-pr-12"},
			want:    cliRequest{Op: "alias", Target: "anna-8cc7", Alias: "review-pr-12"},
		},
		{
			name:    "alias removed",
			command: "alias",
			args:    []string{"--session", "anna-8cc7", ""},
			want:    cliRequest{Op: "alias", Target: "anna-8cc7"},
		},
		{
			name:    "close by name",
			command: "close",
			args:    []string{"anna-8cc7"},
			want:    cliRequest{Op: "close", Target: "anna-8cc7"},
		},
		{
			name:    "tree",
			command: "tree",
			want:    cliRequest{Op: "tree"},
		},
		{
			name:    "gc",
			command: "gc",
			want:    cliRequest{Op: "gc"},
		},
		{
			name:    "gc dry run",
			command: "gc",
			args:    []string{"--dry-run"},
			want:    cliRequest{Op: "gc", DryRun: true},
		},
		{
			name:    "project list",
			command: "project",
			args:    []string{"list"},
			want:    cliRequest{Op: "project", ProjectOp: "list"},
		},
		{
			name:    "project args shown",
			command: "project",
			args:    []string{"args", "/tmp/repo"},
			want:    cliRequest{Op: "project", ProjectOp: "show-args", Dir: "/tmp/repo"},
		},
		{
			name:    "project args set, flags after -- kept as arguments",
			command: "project",
			args:    []string{"args", "/tmp/repo", "--", "--dangerously-load-development-channels", "server:xwb", "--clear"},
			want:    cliRequest{Op: "project", ProjectOp: "set-args", Dir: "/tmp/repo", ClaudeArgs: []string{"--dangerously-load-development-channels", "server:xwb", "--clear"}},
		},
		{
			name:    "project args cleared",
			command: "project",
			args:    []string{"args", "--clear", "/tmp/repo"},
			want:    cliRequest{Op: "project", ProjectOp: "clear-args", Dir: "/tmp/repo"},
		},
		{
			name:    "hook status",
			command: "hook",
			args:    []string{"status", "waiting_approval", "--session", "abc"},
			want:    cliRequest{Op: "hook", HookEvent: "status", Activity: session.ActivityWaitingApproval, Session: "abc"},
		},
		{
			name:    "hook status waiting_action with a reason",
			command: "hook",
			args:    []string{"status", "waiting_action", "--reason", "gh にログインしてください", "--wait-kind", "command", "--session", "abc"},
			want:    cliRequest{Op: "hook", HookEvent: "status", Activity: session.ActivityWaitingAction, StatusReason: "gh にログインしてください", WaitKind: session.WaitCommand, Session: "abc"},
		},
		{
			// A session that still runs an older plugin sends no kind.
			name:    "hook status waiting_action without a wait kind",
			command: "hook",
			args:    []string{"status", "waiting_action", "--reason", "x", "--session", "abc"},
			want:    cliRequest{Op: "hook", HookEvent: "status", Activity: session.ActivityWaitingAction, StatusReason: "x", Session: "abc"},
		},
		{
			name:    "hook status waiting_action for an API error",
			command: "hook",
			args:    []string{"status", "waiting_action", "--reason", "API の誤りで止まりました", "--wait-kind", "api_error", "--session", "abc"},
			want:    cliRequest{Op: "hook", HookEvent: "status", Activity: session.ActivityWaitingAction, StatusReason: "API の誤りで止まりました", WaitKind: session.WaitAPIError, Session: "abc"},
		},
		{
			// The plugin always passes --wait-kind; empty means no wait.
			name:    "hook status with an empty wait kind",
			command: "hook",
			args:    []string{"status", "running", "--reason", "", "--wait-kind", "", "--session", "abc"},
			want:    cliRequest{Op: "hook", HookEvent: "status", Activity: session.ActivityRunning, Session: "abc"},
		},
		{
			name:    "hook context",
			command: "hook",
			args:    []string{"context", "--tokens", "120000", "--window", "200000", "--session", "abc"},
			want:    cliRequest{Op: "hook", HookEvent: "context", ContextTokens: new(120_000), ContextWindow: 200_000, Session: "abc"},
		},
		{
			name:    "hook context with zero tokens",
			command: "hook",
			args:    []string{"context", "--tokens", "0", "--window", "200000", "--session", "abc"},
			want:    cliRequest{Op: "hook", HookEvent: "context", ContextTokens: new(0), ContextWindow: 200_000, Session: "abc"},
		},
		{
			name:    "hook context before the first response",
			command: "hook",
			args:    []string{"context", "--tokens", "", "--window", "1000000", "--session", "abc"},
			want:    cliRequest{Op: "hook", HookEvent: "context", ContextWindow: 1_000_000, Session: "abc"},
		},
		{
			name:    "clear",
			command: "clear",
			args:    []string{"--id", "abc"},
			want:    cliRequest{Op: "send", Target: "abc", Text: "/clear"},
		},
		{
			name:    "clear dry run",
			command: "clear",
			args:    []string{"--id", "anna-8cc7", "--dry-run"},
			want:    cliRequest{Op: "send", Target: "anna-8cc7", Text: "/clear", DryRun: true},
		},
		{
			name:    "send",
			command: "send",
			args:    []string{"--id", "anna-8cc7", "-p", "続けて"},
			want:    cliRequest{Op: "send", Target: "anna-8cc7", Text: "続けて"},
		},
		{
			name:    "send dry run",
			command: "send",
			args:    []string{"--id", "abc", "-p", "/reload-plugins", "--dry-run"},
			want:    cliRequest{Op: "send", Target: "abc", Text: "/reload-plugins", DryRun: true},
		},
		{
			name:    "broadcast to every live session",
			command: "broadcast",
			args:    []string{"-p", "/reload-plugins"},
			want:    cliRequest{Op: "broadcast", Text: "/reload-plugins"},
		},
		{
			name:    "broadcast with every filter",
			command: "broadcast",
			args:    []string{"-p", "/reload-plugins", "--repo", "deck", "--alias", "review", "--only", "anna-8cc7, abc", "--dry-run"},
			want:    cliRequest{Op: "broadcast", Text: "/reload-plugins", Filter: sessionFilter{Repo: "deck", Alias: "review", Only: []string{"anna-8cc7", "abc"}}, DryRun: true},
		},
		{
			name:    "list --only",
			command: "list",
			args:    []string{"--only", "anna-8cc7"},
			want:    cliRequest{Op: "list", Filter: sessionFilter{Only: []string{"anna-8cc7"}}},
		},
		{
			// The plugin always passes --reason; empty means no reason.
			name:    "hook status with an empty reason",
			command: "hook",
			args:    []string{"status", "idle", "--reason", "", "--session", "abc"},
			want:    cliRequest{Op: "hook", HookEvent: "status", Activity: session.ActivityIdle, Session: "abc"},
		},
		{
			name:    "hook session-start",
			command: "hook",
			args:    []string{"session-start", "--claude-session-id", "uuid-1", "--source", "clear", "--session", "abc"},
			want:    cliRequest{Op: "hook", HookEvent: "session-start", ClaudeSessionID: "uuid-1", Source: "clear", Session: "abc"},
		},
		{
			name:    "hook exited",
			command: "hook",
			args:    []string{"exited", "--session", "abc"},
			want:    cliRequest{Op: "hook", HookEvent: "exited", Session: "abc"},
		},
		{
			name:    "hook rate-limits",
			command: "hook",
			args:    []string{"rate-limits", `[{"kind":"five_hour","percentUsed":12}]`, "--session", "abc"},
			want:    cliRequest{Op: "hook", HookEvent: "rate-limits", RateLimits: `[{"kind":"five_hour","percentUsed":12}]`, Session: "abc"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdin := tt.stdin
			if stdin == nil {
				stdin = failingReader{t}
			}
			got, err := cliCommands[tt.command](tt.args, stdin)
			if err != nil {
				t.Fatalf("parse %s %v: %v", tt.command, tt.args, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parse %s %v = %+v, want %+v", tt.command, tt.args, got, tt.want)
			}
		})
	}
}

func TestParseHookArgs_SessionFromEnv(t *testing.T) {
	t.Setenv("CLAUDE_DECK_SESSION_ID", "from-env")
	got, err := parseHookArgs([]string{"exited"})
	if err != nil {
		t.Fatalf("parseHookArgs: %v", err)
	}
	if got.Session != "from-env" {
		t.Errorf("Session = %q, want from-env", got.Session)
	}
}

func TestParseForkArgs_SessionFromEnv(t *testing.T) {
	t.Setenv("CLAUDE_DECK_SESSION_ID", "from-env")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"from the environment", nil, "from-env"},
		{"--session wins", []string{"--session", "anna-8cc7"}, "anna-8cc7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cliCommands["fork"](tt.args, failingReader{t})
			if err != nil {
				t.Fatalf("parse fork: %v", err)
			}
			want := cliRequest{Op: "fork", Target: tt.want}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestParseAliasArgs_SessionFromEnv(t *testing.T) {
	t.Setenv("CLAUDE_DECK_SESSION_ID", "from-env")
	got, err := parseAliasArgs([]string{"review"})
	if err != nil {
		t.Fatalf("parseAliasArgs: %v", err)
	}
	want := cliRequest{Op: "alias", Target: "from-env", Alias: "review"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseCLIArgsRejectsInvalid(t *testing.T) {
	t.Setenv("CLAUDE_DECK_SESSION_ID", "")
	tests := []struct {
		name    string
		command string
		args    []string
		stdin   io.Reader
	}{
		{name: "new with positional argument", command: "new", args: []string{"extra"}},
		{name: "list with positional argument", command: "list", args: []string{"extra"}},
		{name: "close without target", command: "close"},
		{name: "alias without alias", command: "alias", args: []string{"--session", "abc"}},
		{name: "alias with two aliases", command: "alias", args: []string{"--session", "abc", "a", "b"}},
		{name: "alias without session", command: "alias", args: []string{"review"}},
		{name: "close with two targets", command: "close", args: []string{"a", "b"}},
		{name: "unknown flag", command: "new", args: []string{"--bogus"}},
		{name: "new with an empty prompt", command: "new", args: []string{"--prompt", ""}},
		{name: "new with a blank prompt", command: "new", args: []string{"--prompt", " \n\t"}},
		{name: "new with --prompt and no value", command: "new", args: []string{"--prompt"}},
		{name: "new with an empty prompt on stdin", command: "new", args: []string{"--prompt", "-"}, stdin: strings.NewReader("")},
		{name: "new with a blank prompt on stdin", command: "new", args: []string{"--prompt", "-"}, stdin: strings.NewReader(" \n ")},
		{name: "new when stdin fails midway", command: "new", args: []string{"--prompt", "-"}, stdin: io.MultiReader(strings.NewReader("partial"), errReader{})},
		{name: "fork with positional argument", command: "fork", args: []string{"--session", "a", "extra"}},
		{name: "fork without session", command: "fork"},
		{name: "fork with a blank prompt", command: "fork", args: []string{"--session", "a", "--prompt", " \n\t"}},
		{name: "fork with --prompt and no value", command: "fork", args: []string{"--session", "a", "--prompt"}},
		{name: "fork when stdin fails midway", command: "fork", args: []string{"--session", "a", "--prompt", "-"}, stdin: io.MultiReader(strings.NewReader("partial"), errReader{})},
		{name: "gc with positional argument", command: "gc", args: []string{"extra"}},
		{name: "tree with positional argument", command: "tree", args: []string{"extra"}},
		{name: "project without operation", command: "project"},
		{name: "project unknown operation", command: "project", args: []string{"bogus"}},
		{name: "project list with positional argument", command: "project", args: []string{"list", "extra"}},
		{name: "project args without dir", command: "project", args: []string{"args"}},
		{name: "project args without -- before arguments", command: "project", args: []string{"args", "/tmp/repo", "--verbose"}},
		{name: "project args with nothing after --", command: "project", args: []string{"args", "/tmp/repo", "--"}},
		{name: "project args cleared and set", command: "project", args: []string{"args", "--clear", "/tmp/repo", "--", "--verbose"}},
		{name: "hook without event", command: "hook"},
		{name: "hook unknown event", command: "hook", args: []string{"bogus", "--session", "abc"}},
		{name: "hook status without status", command: "hook", args: []string{"status"}},
		{name: "hook status unknown status", command: "hook", args: []string{"status", "busy", "--session", "abc"}},
		{name: "hook status completed", command: "hook", args: []string{"status", "completed", "--session", "abc"}},
		{name: "hook status waiting_action without a reason", command: "hook", args: []string{"status", "waiting_action", "--session", "abc"}},
		{name: "hook status waiting_action with an empty reason", command: "hook", args: []string{"status", "waiting_action", "--reason", "", "--session", "abc"}},
		{name: "hook status waiting_action with a blank reason", command: "hook", args: []string{"status", "waiting_action", "--reason", " ", "--session", "abc"}},
		{name: "hook status waiting_action with --reason and no value", command: "hook", args: []string{"status", "waiting_action", "--reason"}},
		{name: "hook status waiting_action with a line break in the reason", command: "hook", args: []string{"status", "waiting_action", "--reason", "gh に\nログイン", "--wait-kind", "command", "--session", "abc"}},
		{name: "hook status waiting_action with an unknown wait kind", command: "hook", args: []string{"status", "waiting_action", "--reason", "x", "--wait-kind", "bogus", "--session", "abc"}},
		{name: "hook status idle with a wait kind", command: "hook", args: []string{"status", "idle", "--wait-kind", "command", "--session", "abc"}},
		{name: "hook context without a window", command: "hook", args: []string{"context", "--tokens", "1", "--session", "abc"}},
		{name: "hook context with a zero window", command: "hook", args: []string{"context", "--tokens", "1", "--window", "0", "--session", "abc"}},
		{name: "hook context with negative tokens", command: "hook", args: []string{"context", "--tokens", "-1", "--window", "200000", "--session", "abc"}},
		{name: "hook context with tokens that are not a number", command: "hook", args: []string{"context", "--tokens", "many", "--window", "200000", "--session", "abc"}},
		{name: "clear without --id", command: "clear"},
		{name: "clear with an empty --id", command: "clear", args: []string{"--id", ""}},
		{name: "clear with a positional argument", command: "clear", args: []string{"abc"}},
		{name: "clear with an extra positional argument", command: "clear", args: []string{"--id", "abc", "extra"}},
		{name: "clear does not take -p", command: "clear", args: []string{"--id", "abc", "-p", "x"}},
		{name: "send without -p", command: "send", args: []string{"--id", "abc"}},
		{name: "send with a blank -p", command: "send", args: []string{"--id", "abc", "-p", "  "}},
		{name: "send with a line break", command: "send", args: []string{"--id", "abc", "-p", "a\nb"}},
		{name: "send without --id", command: "send", args: []string{"-p", "x"}},
		{name: "send with a positional argument", command: "send", args: []string{"--id", "abc", "-p", "x", "extra"}},
		{name: "broadcast without -p", command: "broadcast"},
		{name: "broadcast with a line break", command: "broadcast", args: []string{"-p", "a\rb"}},
		{name: "broadcast with a positional argument", command: "broadcast", args: []string{"-p", "x", "extra"}},
		{name: "hook status idle with a reason", command: "hook", args: []string{"status", "idle", "--reason", "x", "--session", "abc"}},
		{name: "hook without session", command: "hook", args: []string{"exited"}},
		{name: "hook rate-limits without JSON", command: "hook", args: []string{"rate-limits"}},
		{name: "hook rate-limits with invalid JSON", command: "hook", args: []string{"rate-limits", "five_hour=12", "--session", "abc"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdin := tt.stdin
			if stdin == nil {
				stdin = strings.NewReader("")
			}
			if _, err := cliCommands[tt.command](tt.args, stdin); err == nil {
				t.Errorf("parse %s %v: want error, got nil", tt.command, tt.args)
			}
		})
	}
}

// work_dir is where the session runs, or the repository while its workspace
// is removed.
func TestInfoFromSnapshot_WorkDir(t *testing.T) {
	tests := []struct {
		name string
		iso  session.Isolation
		want string
	}{
		{"direct", session.DirectIsolation("/repo/sub"), "/repo/sub"},
		{"workspace subdir", session.WorkspaceIsolation("/data/ws/sub"), "/data/ws/sub"},
		{"removed workspace", session.WorkspaceIsolation("/data/ws").Removed("a", "b"), "/repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := infoFromSnapshot(session.Snapshot{ID: "s", RepoPath: "/repo", Isolation: tt.iso}, nil).WorkDir
			if got != tt.want {
				t.Errorf("work_dir = %q, want %q", got, tt.want)
			}
		})
	}
}

// failingReader fails the test when read: a command that was not asked to read
// stdin must not block on it.
type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Error("stdin was read")
	return 0, io.EOF
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
