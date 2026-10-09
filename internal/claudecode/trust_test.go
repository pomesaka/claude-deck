package claudecode

import (
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureTrusted(t *testing.T) {
	const dir = "/data/workspace/repo/ws"

	tests := []struct {
		name   string
		before string // ~/.claude.json の内容。空ならファイルなし
		want   string
	}{
		{
			name:   "no config file",
			before: "",
			want:   `{"projects":{"/data/workspace/repo/ws":{"hasTrustDialogAccepted":true}}}`,
		},
		{
			name:   "no entry for the directory keeps the rest of the file",
			before: `{"numStartups":3,"projects":{"/other":{"hasTrustDialogAccepted":false}}}`,
			want:   `{"numStartups":3,"projects":{"/other":{"hasTrustDialogAccepted":false},"/data/workspace/repo/ws":{"hasTrustDialogAccepted":true}}}`,
		},
		{
			name:   "untrusted entry keeps its other keys",
			before: `{"projects":{"/data/workspace/repo/ws":{"allowedTools":["Bash"],"hasTrustDialogAccepted":false}}}`,
			want:   `{"projects":{"/data/workspace/repo/ws":{"allowedTools":["Bash"],"hasTrustDialogAccepted":true}}}`,
		},
		{
			name:   "trusted entry",
			before: `{"projects":{"/data/workspace/repo/ws":{"hasTrustDialogAccepted":true}}}`,
			want:   `{"projects":{"/data/workspace/repo/ws":{"hasTrustDialogAccepted":true}}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			cfgPath := filepath.Join(home, ".claude.json")
			if tt.before != "" {
				if err := os.WriteFile(cfgPath, []byte(tt.before), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if err := EnsureTrusted(dir); err != nil {
				t.Fatalf("EnsureTrusted: %v", err)
			}

			got, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			var gotV, wantV any
			if err := json.Unmarshal(got, &gotV); err != nil {
				t.Fatalf("parsing written config: %v", err)
			}
			if err := json.Unmarshal([]byte(tt.want), &wantV); err != nil {
				t.Fatal(err)
			}
			gotJSON, _ := json.Marshal(gotV, json.Deterministic(true))
			wantJSON, _ := json.Marshal(wantV, json.Deterministic(true))
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("config = %s, want %s", gotJSON, wantJSON)
			}
			info, err := os.Stat(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("mode = %v, want 0600", info.Mode().Perm())
			}
		})
	}
}

func TestForgetProjects(t *testing.T) {
	tests := []struct {
		name   string
		before string
		dryRun bool
		want   string
		wantN  int
	}{
		{
			name:   "removes the matching entries",
			before: `{"numStartups":3,"projects":{"/data/ws/a":{"x":1},"/data/ws/a/sub":{"x":2},"/data/ws/ab":{"x":3},"/repo":{"x":4}}}`,
			want:   `{"numStartups":3,"projects":{"/data/ws/ab":{"x":3},"/repo":{"x":4}}}`,
			wantN:  2,
		},
		{
			name:   "dry run only counts",
			before: `{"projects":{"/data/ws/a":{"x":1},"/data/ws/a/sub":{"x":2},"/repo":{"x":4}}}`,
			dryRun: true,
			want:   `{"projects":{"/data/ws/a":{"x":1},"/data/ws/a/sub":{"x":2},"/repo":{"x":4}}}`,
			wantN:  2,
		},
		{
			name:   "no entry",
			before: `{"projects":{"/repo":{"x":4}}}`,
			want:   `{"projects":{"/repo":{"x":4}}}`,
			wantN:  0,
		},
	}
	match := func(dir string) bool { return dir == "/data/ws/a" || strings.HasPrefix(dir, "/data/ws/a/") }
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			cfgPath := filepath.Join(home, ".claude.json")
			if err := os.WriteFile(cfgPath, []byte(tt.before), 0o600); err != nil {
				t.Fatal(err)
			}

			n, err := ForgetProjects(match, tt.dryRun)
			if err != nil {
				t.Fatalf("ForgetProjects: %v", err)
			}
			if n != tt.wantN {
				t.Errorf("n = %d, want %d", n, tt.wantN)
			}

			got, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			var gotV, wantV any
			if err := json.Unmarshal(got, &gotV); err != nil {
				t.Fatalf("parsing written config: %v", err)
			}
			if err := json.Unmarshal([]byte(tt.want), &wantV); err != nil {
				t.Fatal(err)
			}
			gotJSON, _ := json.Marshal(gotV, json.Deterministic(true))
			wantJSON, _ := json.Marshal(wantV, json.Deterministic(true))
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("config = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// A trusted entry leaves the file alone: Claude Code sessions that are running
// rewrite ~/.claude.json too, and every write here can race with theirs.
func TestEnsureTrusted_LeavesTrustedFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(home, ".claude.json")
	before := `{"projects":{"/ws":{"hasTrustDialogAccepted":true}}}`
	if err := os.WriteFile(cfgPath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureTrusted("/ws"); err != nil {
		t.Fatalf("EnsureTrusted: %v", err)
	}
	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != before {
		t.Errorf("file was rewritten: %s", got)
	}
}
