package main

import (
	"os"
	"testing"
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
			name:    "list",
			command: "list",
			want:    cliRequest{Op: "list"},
		},
		{
			name:    "list filtered by alias",
			command: "list",
			args:    []string{"--alias", "review"},
			want:    cliRequest{Op: "list", Alias: "review"},
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
			name:    "hook status",
			command: "hook",
			args:    []string{"status", "waiting_approval", "--session", "abc"},
			want:    cliRequest{Op: "hook", HookEvent: "status", Status: "waiting_approval", Session: "abc"},
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
			got, err := cliCommands[tt.command](tt.args)
			if err != nil {
				t.Fatalf("parse %s %v: %v", tt.command, tt.args, err)
			}
			if got != tt.want {
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

func TestAliasMatches(t *testing.T) {
	tests := []struct {
		alias, filter string
		want          bool
	}{
		{"review-pr-12", "", true},
		{"", "", true},
		{"review-pr-12", "review-pr-12", true},
		{"review-pr-12", "pr-12", true},
		{"review-pr-12", "Review", true},
		{"review-pr-12", "fix", false},
		{"", "review", false},
	}
	for _, tt := range tests {
		if got := aliasMatches(tt.alias, tt.filter); got != tt.want {
			t.Errorf("aliasMatches(%q, %q) = %v, want %v", tt.alias, tt.filter, got, tt.want)
		}
	}
}

func TestParseAliasArgs_SessionFromEnv(t *testing.T) {
	t.Setenv("CLAUDE_DECK_SESSION_ID", "from-env")
	got, err := parseAliasArgs([]string{"review"})
	if err != nil {
		t.Fatalf("parseAliasArgs: %v", err)
	}
	want := cliRequest{Op: "alias", Target: "from-env", Alias: "review"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseCLIArgsRejectsInvalid(t *testing.T) {
	t.Setenv("CLAUDE_DECK_SESSION_ID", "")
	tests := []struct {
		name    string
		command string
		args    []string
	}{
		{name: "new with positional argument", command: "new", args: []string{"extra"}},
		{name: "list with positional argument", command: "list", args: []string{"extra"}},
		{name: "close without target", command: "close"},
		{name: "alias without alias", command: "alias", args: []string{"--session", "abc"}},
		{name: "alias with two aliases", command: "alias", args: []string{"--session", "abc", "a", "b"}},
		{name: "alias without session", command: "alias", args: []string{"review"}},
		{name: "close with two targets", command: "close", args: []string{"a", "b"}},
		{name: "unknown flag", command: "new", args: []string{"--prompt", "hi"}},
		{name: "gc with positional argument", command: "gc", args: []string{"extra"}},
		{name: "tree with positional argument", command: "tree", args: []string{"extra"}},
		{name: "hook without event", command: "hook"},
		{name: "hook unknown event", command: "hook", args: []string{"bogus", "--session", "abc"}},
		{name: "hook status without status", command: "hook", args: []string{"status"}},
		{name: "hook status unknown status", command: "hook", args: []string{"status", "busy", "--session", "abc"}},
		{name: "hook status completed", command: "hook", args: []string{"status", "completed", "--session", "abc"}},
		{name: "hook without session", command: "hook", args: []string{"exited"}},
		{name: "hook rate-limits without JSON", command: "hook", args: []string{"rate-limits"}},
		{name: "hook rate-limits with invalid JSON", command: "hook", args: []string{"rate-limits", "five_hour=12", "--session", "abc"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := cliCommands[tt.command](tt.args); err == nil {
				t.Errorf("parse %s %v: want error, got nil", tt.command, tt.args)
			}
		})
	}
}
