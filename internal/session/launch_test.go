package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveLaunchDir(t *testing.T) {
	root := t.TempDir()
	mainRepo := filepath.Join(root, "repo")
	workspace := filepath.Join(root, "ws", "anna-8cc7")
	plain := filepath.Join(root, "plain")

	mustMkdir(t, filepath.Join(mainRepo, ".jj", "repo"))
	mustMkdir(t, filepath.Join(mainRepo, "apps", "web"))
	// jj ワークスペースの .jj/repo は本体の .jj/repo を指すファイル
	mustMkdir(t, filepath.Join(workspace, ".jj"))
	mustMkdir(t, filepath.Join(workspace, "apps", "web"))
	if err := os.WriteFile(filepath.Join(workspace, ".jj", "repo"), []byte(filepath.Join(mainRepo, ".jj", "repo")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, plain)

	tests := []struct {
		name           string
		dir            string
		wantRepoPath   string
		wantWorkingDir string
		wantIsJJ       bool
	}{
		{
			name:           "main repo root",
			dir:            mainRepo,
			wantRepoPath:   mainRepo,
			wantWorkingDir: mainRepo,
			wantIsJJ:       true,
		},
		{
			name:           "sub project in main repo",
			dir:            filepath.Join(mainRepo, "apps", "web"),
			wantRepoPath:   mainRepo,
			wantWorkingDir: filepath.Join(mainRepo, "apps", "web"),
			wantIsJJ:       true,
		},
		{
			name:           "workspace root resolves to main repo",
			dir:            workspace,
			wantRepoPath:   mainRepo,
			wantWorkingDir: mainRepo,
			wantIsJJ:       true,
		},
		{
			name:           "sub project in workspace keeps relative position",
			dir:            filepath.Join(workspace, "apps", "web"),
			wantRepoPath:   mainRepo,
			wantWorkingDir: filepath.Join(mainRepo, "apps", "web"),
			wantIsJJ:       true,
		},
		{
			name:           "non jj directory",
			dir:            plain,
			wantRepoPath:   plain,
			wantWorkingDir: plain,
			wantIsJJ:       false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoPath, workingDir, isJJ := ResolveLaunchDir(tt.dir)
			if repoPath != tt.wantRepoPath || workingDir != tt.wantWorkingDir || isJJ != tt.wantIsJJ {
				t.Errorf("ResolveLaunchDir(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.dir, repoPath, workingDir, isJJ, tt.wantRepoPath, tt.wantWorkingDir, tt.wantIsJJ)
			}
		})
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestFindSession(t *testing.T) {
	m := newTestManager()
	for _, s := range []struct {
		id   DeckSessionID
		name string
	}{
		{"id-unique", "anna-8cc7"},
		{"id-dup-1", "abcd1234"},
		{"id-dup-2", "abcd1234"},
	} {
		sess := NewSession("/repo", "repo")
		sess.ID = s.id
		sess.Name = s.name
		m.sessions[s.id] = sess
	}

	tests := []struct {
		name    string
		key     string
		wantID  DeckSessionID
		wantErr bool
	}{
		{name: "by id", key: "id-dup-1", wantID: "id-dup-1"},
		{name: "by unique name", key: "anna-8cc7", wantID: "id-unique"},
		{name: "ambiguous name", key: "abcd1234", wantErr: true},
		{name: "not found", key: "nope", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := m.FindSession(tt.key)
			if (err != nil) != tt.wantErr {
				t.Fatalf("FindSession(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
			}
			if err == nil && got.ID != tt.wantID {
				t.Errorf("FindSession(%q) = %s, want %s", tt.key, got.ID, tt.wantID)
			}
		})
	}
}
