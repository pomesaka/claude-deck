package agentruntime

import (
	"reflect"
	"testing"
)

func TestClaudeRuntimeStartSpec(t *testing.T) {
	tests := []struct {
		name string
		req  StartRequest
		want StartSpec
	}{
		{
			name: "new interactive session",
			req: StartRequest{
				Mode:           LaunchNew,
				SessionName:    "emiri-1234",
				PermissionMode: "default",
				AdditionalArgs: []string{"--add-dir", "/repo/shared"},
			},
			want: StartSpec{
				Command: "claude",
				Args: []string{
					"--permission-mode", "default",
					"--agent", "emiri-1234",
					"--add-dir", "/repo/shared",
				},
			},
		},
		{
			name: "resume",
			req: StartRequest{
				Mode:           LaunchResume,
				SessionID:      "runtime-abc",
				PermissionMode: "acceptEdits",
			},
			want: StartSpec{
				Command: "claude",
				Args: []string{
					"--resume", "runtime-abc",
					"--permission-mode", "acceptEdits",
				},
			},
		},
		{
			name: "fork",
			req: StartRequest{
				Mode:        LaunchFork,
				SessionID:   "runtime-abc",
				SessionName: "forked",
			},
			want: StartSpec{
				Command: "claude",
				Args: []string{
					"--resume", "runtime-abc",
					"--fork-session",
					"--agent", "forked",
				},
			},
		},
		{
			name: "new prompt",
			req: StartRequest{
				Mode:   LaunchNew,
				Prompt: "hello",
			},
			want: StartSpec{
				Command: "claude",
				Args:    []string{"-p", "hello"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (ClaudeRuntime{Command: "claude"}).StartSpec(tt.req)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("StartSpec() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
