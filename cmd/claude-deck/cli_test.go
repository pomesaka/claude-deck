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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := cliCommands[tt.command](tt.args); err == nil {
				t.Errorf("parse %s %v: want error, got nil", tt.command, tt.args)
			}
		})
	}
}
