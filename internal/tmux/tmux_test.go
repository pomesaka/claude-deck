package tmux

import "testing"

func TestNewWindowArgs(t *testing.T) {
	tests := []struct {
		name        string
		sessionName string
		windowName  string
		opts        WindowOpts
		want        []string
	}{
		{
			name:        "minimal_no_options",
			sessionName: "claude-deck",
			windowName:  "abc123",
			opts:        WindowOpts{},
			want:        []string{"new-window", "-t", "claude-deck:", "-n", "abc123"},
		},
		{
			name:        "with_command_only",
			sessionName: "claude-deck",
			windowName:  "sess1",
			opts:        WindowOpts{Command: "claude --agent sess1"},
			want:        []string{"new-window", "-t", "claude-deck:", "-n", "sess1", "claude --agent sess1"},
		},
		{
			name:        "with_workdir_only",
			sessionName: "claude-deck",
			windowName:  "sess2",
			opts:        WindowOpts{WorkDir: "/home/user/project"},
			want:        []string{"new-window", "-t", "claude-deck:", "-n", "sess2", "-c", "/home/user/project"},
		},
		{
			name:        "with_single_env",
			sessionName: "claude-deck",
			windowName:  "sess3",
			opts:        WindowOpts{Env: []string{"CLAUDE_DECK_SESSION_ID=abc"}},
			want:        []string{"new-window", "-t", "claude-deck:", "-n", "sess3", "-e", "CLAUDE_DECK_SESSION_ID=abc"},
		},
		{
			name:        "with_multiple_env",
			sessionName: "claude-deck",
			windowName:  "sess4",
			opts:        WindowOpts{Env: []string{"FOO=bar", "BAZ=qux"}},
			want:        []string{"new-window", "-t", "claude-deck:", "-n", "sess4", "-e", "FOO=bar", "-e", "BAZ=qux"},
		},
		{
			name:        "full_options",
			sessionName: "my-session",
			windowName:  "full",
			opts: WindowOpts{
				Command: "claude",
				WorkDir: "/tmp/project",
				Env:     []string{"KEY=val"},
			},
			// order: target, name, workdir, env..., command
			want: []string{"new-window", "-t", "my-session:", "-n", "full", "-c", "/tmp/project", "-e", "KEY=val", "claude"},
		},
		{
			name:        "session_name_colon_suffix",
			sessionName: "my-custom-session",
			windowName:  "win",
			opts:        WindowOpts{},
			// target must be "session:" (with trailing colon) to append to that session
			want: []string{"new-window", "-t", "my-custom-session:", "-n", "win"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := newWindowArgs(tc.sessionName, tc.windowName, tc.opts)
			if len(got) != len(tc.want) {
				t.Fatalf("arg count mismatch: got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("arg[%d]: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseWindowList(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []WindowInfo
	}{
		{
			name: "empty_output",
			out:  "",
			want: nil,
		},
		{
			name: "blank_line_only",
			out:  "\n",
			want: nil,
		},
		{
			name: "single_window",
			out:  "abc123 12345",
			want: []WindowInfo{{Name: "abc123", PanePID: 12345}},
		},
		{
			name: "multiple_windows",
			out:  "sess1 100\nsess2 200\nsess3 300",
			want: []WindowInfo{
				{Name: "sess1", PanePID: 100},
				{Name: "sess2", PanePID: 200},
				{Name: "sess3", PanePID: 300},
			},
		},
		{
			name: "trailing_newline",
			out:  "sess1 100\n",
			want: []WindowInfo{{Name: "sess1", PanePID: 100}},
		},
		{
			name: "zero_pid",
			out:  "noprocess 0",
			want: []WindowInfo{{Name: "noprocess", PanePID: 0}},
		},
		{
			name: "embedded_blank_lines",
			out:  "win1 1\n\nwin2 2",
			want: []WindowInfo{
				{Name: "win1", PanePID: 1},
				{Name: "win2", PanePID: 2},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseWindowList(tc.out)
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d]: got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestExitChannel(t *testing.T) {
	tests := []struct {
		windowName string
		want       string
	}{
		{"abc123", "deck-exit-abc123"},
		{"session-id-xyz", "deck-exit-session-id-xyz"},
		{"a1b2c3d4e5f6", "deck-exit-a1b2c3d4e5f6"},
	}
	for _, tc := range tests {
		t.Run(tc.windowName, func(t *testing.T) {
			if got := ExitChannel(tc.windowName); got != tc.want {
				t.Errorf("ExitChannel(%q) = %q, want %q", tc.windowName, got, tc.want)
			}
		})
	}
}

func TestRunnerDefaults(t *testing.T) {
	r := &Runner{}
	if got := r.cmd(); got != DefaultCommand {
		t.Errorf("cmd() = %q, want %q", got, DefaultCommand)
	}
	if got := r.sess(); got != DefaultSessionName {
		t.Errorf("sess() = %q, want %q", got, DefaultSessionName)
	}
}

func TestRunnerCustomValues(t *testing.T) {
	r := &Runner{Command: "/usr/local/bin/tmux", SessionName: "my-project"}
	if got := r.cmd(); got != "/usr/local/bin/tmux" {
		t.Errorf("cmd() = %q, want /usr/local/bin/tmux", got)
	}
	if got := r.sess(); got != "my-project" {
		t.Errorf("sess() = %q, want my-project", got)
	}
}
