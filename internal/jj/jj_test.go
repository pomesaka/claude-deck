package jj

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// realJJRepo creates a jj repository with the workspaces named in names, using
// the jj on PATH with an empty user config. It skips the test without jj.
func realJJRepo(t *testing.T, names ...string) (repoPath string, wsPaths map[string]string) {
	t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj is not on PATH")
	}
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("user.name = \"t\"\nuser.email = \"t@example.com\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JJ_CONFIG", cfg)
	root := t.TempDir()
	repoPath = filepath.Join(root, "repo")
	runJJ(t, root, "git", "init", repoPath)
	if err := os.Mkdir(filepath.Join(root, "ws"), 0o755); err != nil {
		t.Fatal(err)
	}
	wsPaths = map[string]string{}
	for _, n := range names {
		wsPaths[n] = filepath.Join(root, "ws", n)
		runJJ(t, repoPath, "workspace", "add", "--name", n, wsPaths[n])
	}
	return repoPath, wsPaths
}

func runJJ(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("jj", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jj %v: %v: %s", args, err, out)
	}
	return string(out)
}

// claude-deck の jj の操作が同じリポジトリで並んで走ると、jj の操作ログが分岐する。
// jj 0.37 は分岐を merge した後の操作ログで作業コピーの操作を祖先と見つけられないことがあり、
// 作業コピーが「sibling of the working copy's operation」で使えなくなる
// （jj-vcs/jj#9314、v0.41.0 で直った）。Runner がリポジトリごとに jj を 1 つずつ通すので、
// 別のプロセスから続けて close や new をしても、操作ログは分岐しない。
func TestRunner_ConcurrentOperationsDoNotDiverge(t *testing.T) {
	names := []string{"w1", "w2", "w3", "w4", "w5", "w6"}
	tests := []struct {
		name string
		// op is what one session does to the workspace name, all of them at once.
		op func(r *Runner, repoPath, name, wsPath string) error
	}{
		{
			name: "closes",
			op: func(r *Runner, repoPath, name, wsPath string) error {
				if _, _, err := r.GetWorkspaceRevisions(wsPath); err != nil {
					return err
				}
				return r.ForgetWorkspace(repoPath, name)
			},
		},
		{
			name: "creates",
			op: func(r *Runner, repoPath, name, wsPath string) error {
				return r.CreateWorkspaceAt(repoPath, "new-"+name, wsPath+"-new", WorkspaceOptions{})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoPath, wsPaths := realJJRepo(t, names...)
			var wg sync.WaitGroup
			errs := make([]error, len(names))
			for i, n := range names {
				wg.Go(func() {
					// 別々の Runner にして、プロセスをまたいだ排他と同じ条件にする。
					errs[i] = tt.op(&Runner{}, repoPath, n, wsPaths[n])
				})
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Errorf("%s: %v", names[i], err)
				}
			}
			ops := runJJ(t, repoPath, "--ignore-working-copy", "op", "log", "--no-graph", "-T", "description ++ \"\\n\"")
			if strings.Contains(ops, "reconcile divergent operations") {
				t.Errorf("the operation log diverged:\n%s", ops)
			}
			if out := runJJ(t, repoPath, "status"); strings.Contains(out, "sibling") {
				t.Errorf("jj status in the repository: %s", out)
			}
		})
	}
}

func TestLockRepo(t *testing.T) {
	tests := []struct {
		name string
		wait time.Duration
		// release is how long the first holder keeps the lock.
		release  time.Duration
		wantWait bool // whether the second one waits for the release
	}{
		{name: "the second one waits for the first", wait: time.Minute, release: 200 * time.Millisecond, wantWait: true},
		{name: "past lockWait the second one runs anyway", wait: 50 * time.Millisecond, release: time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoPath, wsPaths := realJJRepo(t, "w1")
			old := lockWait
			lockWait = tt.wait
			t.Cleanup(func() { lockWait = old })

			unlock := lockRepo(repoPath)
			timer := time.AfterFunc(tt.release, unlock)
			defer func() {
				if timer.Stop() {
					unlock()
				}
			}()
			start := time.Now()
			lockRepo(wsPaths["w1"])() // a workspace shares the lock of its repository
			waited := time.Since(start) >= tt.release
			if waited != tt.wantWait {
				t.Errorf("waited %v for a lock released after %v, want waiting = %v", time.Since(start), tt.release, tt.wantWait)
			}
		})
	}
}

func TestRunner_WorkspaceChanges(t *testing.T) {
	_, ws := realJJRepo(t, "w")
	dir := ws["w"]
	r := &Runner{}
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	steps := []struct {
		name        string
		do          func()
		wantFiles   int
		wantCommits int
	}{
		{"a new workspace holds nothing", func() {}, 0, 0},
		// The session's own jj snapshots the files; until then they are not counted.
		{"files not yet snapshotted are not counted", func() { write("a"); write("b") }, 0, 0},
		{"snapshotted files are counted", func() { runJJ(t, dir, "status") }, 2, 0},
		{"a commit is counted once it is @-", func() { runJJ(t, dir, "commit", "-m", "x") }, 0, 1},
		{"an empty commit is not counted", func() { runJJ(t, dir, "new") }, 0, 1},
		// A remote bookmark on the commit: it was pushed.
		{"a pushed commit is not counted", func() {
			runJJ(t, dir, "git", "remote", "add", "origin", t.TempDir())
			runJJ(t, dir, "bookmark", "create", "feat", "-r", "@-")
			runJJ(t, dir, "bookmark", "track", "feat@origin")
		}, 0, 1},
	}
	for _, st := range steps {
		st.do()
		ops := runJJ(t, dir, "--ignore-working-copy", "op", "log", "--no-graph", "-T", `id ++ "\n"`)
		files, commits, err := r.WorkspaceChanges(dir)
		if err != nil {
			t.Fatalf("%s: WorkspaceChanges: %v", st.name, err)
		}
		if files != st.wantFiles || commits != st.wantCommits {
			t.Errorf("%s: WorkspaceChanges() = (%d, %d), want (%d, %d)", st.name, files, commits, st.wantFiles, st.wantCommits)
		}
		if after := runJJ(t, dir, "--ignore-working-copy", "op", "log", "--no-graph", "-T", `id ++ "\n"`); after != ops {
			t.Errorf("%s: WorkspaceChanges wrote a jj operation", st.name)
		}
	}
	if _, _, err := r.WorkspaceChanges(t.TempDir()); err == nil {
		t.Error("WorkspaceChanges outside a repository succeeded")
	}
}
