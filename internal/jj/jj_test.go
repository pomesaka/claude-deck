package jj

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeJJ writes a script standing in for jj: it logs each call, creates the
// directory on "workspace add", and fails the commands whose first two
// arguments are listed in failing.
func fakeJJ(t *testing.T, failing ...string) (command, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	script.WriteString("echo \"$*\" >> '" + logPath + "'\n")
	script.WriteString("case \"$1 $2\" in\n")
	for _, f := range failing {
		script.WriteString("'" + f + "') exit 1 ;;\n")
	}
	// jj workspace add --name NAME PATH
	script.WriteString("'workspace add') mkdir -p \"$5\" ;;\n")
	script.WriteString("esac\n")
	command = filepath.Join(dir, "jj")
	if err := os.WriteFile(command, []byte(script.String()), 0o755); err != nil {
		t.Fatalf("writing fake jj: %v", err)
	}
	return command, logPath
}

func TestCreateWorkspaceAt_LeftoverAfterFailure(t *testing.T) {
	tests := []struct {
		name       string
		failing    []string
		wantErr    bool
		wantDir    bool
		wantForget bool
	}{
		{name: "success keeps the workspace", wantDir: true},
		{name: "a failure after the add removes the workspace", failing: []string{"new trunk()"}, wantErr: true, wantForget: true},
		{name: "a failed add forgets nothing", failing: []string{"workspace add"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command, logPath := fakeJJ(t, tt.failing...)
			wsPath := filepath.Join(t.TempDir(), "ws")

			err := (&Runner{Command: command}).CreateWorkspaceAt(t.TempDir(), "ws", wsPath, WorkspaceOptions{})
			if (err != nil) != tt.wantErr {
				t.Fatalf("CreateWorkspaceAt error = %v, wantErr %v", err, tt.wantErr)
			}
			if _, statErr := os.Stat(wsPath); (statErr == nil) != tt.wantDir {
				t.Errorf("workspace directory exists = %v, want %v", statErr == nil, tt.wantDir)
			}
			calls, _ := os.ReadFile(logPath)
			if got := strings.Contains(string(calls), "workspace forget"); got != tt.wantForget {
				t.Errorf("workspace forget called = %v, want %v; calls:\n%s", got, tt.wantForget, calls)
			}
		})
	}
}
