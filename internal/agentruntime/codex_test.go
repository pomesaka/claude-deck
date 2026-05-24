package agentruntime

import (
	"reflect"
	"testing"
)

func TestCodexRuntimeStartSpec(t *testing.T) {
	tests := []struct {
		name string
		req  StartRequest
		want StartSpec
	}{
		{
			name: "new interactive session",
			req: StartRequest{
				Mode:           LaunchNew,
				WorkDir:        "/repo/app",
				PermissionMode: "default",
				AdditionalArgs: []string{"--add-dir", "/repo/shared"},
			},
			want: StartSpec{
				Command: "codex",
				Args:    []string{"--cd", "/repo/app", "--sandbox", "workspace-write", "--add-dir", "/repo/shared"},
			},
		},
		{
			name: "add-dir keeps explicit sandbox",
			req: StartRequest{
				Mode:           LaunchNew,
				WorkDir:        "/repo/app",
				PermissionMode: "default",
				AdditionalArgs: []string{"--sandbox", "danger-full-access", "--add-dir", "/repo/shared"},
			},
			want: StartSpec{
				Command: "codex",
				Args: []string{
					"--cd", "/repo/app",
					"--sandbox", "danger-full-access",
					"--add-dir", "/repo/shared",
				},
			},
		},
		{
			name: "resume",
			req: StartRequest{
				Mode:           LaunchResume,
				SessionID:      "runtime-abc",
				WorkDir:        "/repo/app",
				PermissionMode: "on-request",
			},
			want: StartSpec{
				Command: "codex",
				Args: []string{
					"--cd", "/repo/app",
					"--ask-for-approval", "on-request",
					"resume", "runtime-abc",
				},
			},
		},
		{
			name: "fork",
			req: StartRequest{
				Mode:      LaunchFork,
				SessionID: "runtime-abc",
			},
			want: StartSpec{
				Command: "codex",
				Args:    []string{"fork", "runtime-abc"},
			},
		},
		{
			name: "new prompt",
			req: StartRequest{
				Mode:   LaunchNew,
				Prompt: "hello",
			},
			want: StartSpec{
				Command: "codex",
				Args:    []string{"hello"},
			},
		},
		{
			name: "resume prompt",
			req: StartRequest{
				Mode:      LaunchResume,
				SessionID: "runtime-abc",
				Prompt:    "continue",
			},
			want: StartSpec{
				Command: "codex",
				Args:    []string{"resume", "runtime-abc", "continue"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (CodexRuntime{Command: "codex"}).StartSpec(tt.req)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("StartSpec() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
