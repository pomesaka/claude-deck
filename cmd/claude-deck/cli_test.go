package main

import (
	"os"
	"testing"

	"github.com/pomesaka/claude-deck/internal/control"
)

func TestParseCLIArgs(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		command string
		args    []string
		want    control.Request
	}{
		{
			name:    "new defaults to working directory with workspace",
			command: "new",
			want:    control.Request{Op: control.OpNew, Dir: wd},
		},
		{
			name:    "new with dir and no workspace",
			command: "new",
			args:    []string{"--dir", "/tmp/repo", "--no-workspace"},
			want:    control.Request{Op: control.OpNew, Dir: "/tmp/repo", NoWorkspace: true},
		},
		{
			name:    "list",
			command: "list",
			want:    control.Request{Op: control.OpList},
		},
		{
			name:    "close by name",
			command: "close",
			args:    []string{"anna-8cc7"},
			want:    control.Request{Op: control.OpClose, Target: "anna-8cc7"},
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

func TestParseCLIArgsRejectsInvalid(t *testing.T) {
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := cliCommands[tt.command](tt.args); err == nil {
				t.Errorf("parse %s %v: want error, got nil", tt.command, tt.args)
			}
		})
	}
}
