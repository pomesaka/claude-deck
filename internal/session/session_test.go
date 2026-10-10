package session

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStatus_String(t *testing.T) {
	tests := []struct {
		status Status
		want   string
	}{
		{StatusRunning, "Running"},
		{StatusWaitingApproval, "Approve待ち"},
		{StatusWaitingAnswer, "質問待ち"},
		{StatusCompleted, "完了"},
		{StatusError, "エラー"},
		{StatusIdle, "アイドル"},
		{StatusUnmanaged, "外部"},
		{StatusSubagentRunning, "サブエージェント実行中"},
		{Status(99), "Unknown"},
	}
	for _, tt := range tests {
		if got := tt.status.String(); got != tt.want {
			t.Errorf("Status(%d).String() = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestStatus_IsTerminal(t *testing.T) {
	tests := []struct {
		status Status
		want   bool
	}{
		{StatusRunning, false},
		{StatusWaitingApproval, false},
		{StatusWaitingAnswer, false},
		{StatusCompleted, true},
		{StatusError, true},
		{StatusIdle, false},
		{StatusUnmanaged, false},
		{StatusSubagentRunning, false},
	}
	for _, tt := range tests {
		if got := tt.status.IsTerminal(); got != tt.want {
			t.Errorf("Status(%d).IsTerminal() = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestStatus_NeedsAttention(t *testing.T) {
	tests := []struct {
		status Status
		want   bool
	}{
		{StatusRunning, false},
		{StatusWaitingApproval, true},
		{StatusWaitingAnswer, true},
		{StatusCompleted, false},
		{StatusError, false},
		{StatusIdle, false},
		{StatusUnmanaged, false},
		{StatusSubagentRunning, false},
	}
	for _, tt := range tests {
		if got := tt.status.NeedsAttention(); got != tt.want {
			t.Errorf("Status(%d).NeedsAttention() = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestNewSession(t *testing.T) {
	sess := NewSession("/repo", "my-repo")

	if sess.ID == "" {
		t.Error("expected non-empty ID")
	}
	if sess.Name == "" {
		t.Error("expected non-empty Name")
	}
	if sess.RepoPath != "/repo" {
		t.Errorf("RepoPath = %q, want /repo", sess.RepoPath)
	}
	if sess.RepoName != "my-repo" {
		t.Errorf("RepoName = %q, want my-repo", sess.RepoName)
	}
	if sess.Status != StatusIdle {
		t.Errorf("Status = %v, want StatusIdle", sess.Status)
	}
	if sess.StartedAt.IsZero() {
		t.Error("expected non-zero StartedAt")
	}
}

func TestGenerateSessionID(t *testing.T) {
	id1 := GenerateSessionID()
	id2 := GenerateSessionID()
	if id1 == id2 {
		t.Error("expected different session IDs")
	}
	if len(id1) != 16 {
		t.Errorf("expected 16 hex chars, got %d", len(id1))
	}
}

func TestGenerateWorkspaceName(t *testing.T) {
	name := GenerateWorkspaceName()
	if !strings.Contains(name, "-") {
		t.Errorf("expected name with dash, got %q", name)
	}
}

func TestSession_SetCurrentTool(t *testing.T) {
	sess := NewSession("/repo", "repo")
	sess.SetCurrentTool("bash")

	snap := sess.Snapshot()
	if snap.CurrentTool != "bash" {
		t.Errorf("CurrentTool = %q, want 'bash'", snap.CurrentTool)
	}
}

func TestSession_Elapsed_Running(t *testing.T) {
	sess := NewSession("/repo", "repo")
	sess.StartedAt = time.Now().Add(-5 * time.Second)

	elapsed := sess.Elapsed()
	if elapsed < 4*time.Second || elapsed > 6*time.Second {
		t.Errorf("expected ~5s elapsed, got %v", elapsed)
	}
}

func TestSession_Elapsed_Completed(t *testing.T) {
	sess := NewSession("/repo", "repo")
	start := time.Now().Add(-10 * time.Second)
	finish := start.Add(5 * time.Second)
	sess.StartedAt = start
	sess.FinishedAt = &finish

	elapsed := sess.Elapsed()
	if elapsed != 5*time.Second {
		t.Errorf("expected 5s elapsed, got %v", elapsed)
	}
}

func TestSession_Snapshot(t *testing.T) {
	sess := NewSession("/repo", "my-repo")
	sess.SetCurrentTool("read")

	snap := sess.Snapshot()
	if snap.ID != sess.ID {
		t.Error("snapshot ID mismatch")
	}
	if snap.RepoName != "my-repo" {
		t.Errorf("snapshot RepoName = %q", snap.RepoName)
	}
	if snap.CurrentTool != "read" {
		t.Errorf("snapshot CurrentTool = %q", snap.CurrentTool)
	}
}

func TestSession_ConcurrentAccess(t *testing.T) {
	sess := NewSession("/repo", "repo")

	var wg sync.WaitGroup
	wg.Add(3)

	// Writer goroutine
	go func() {
		defer wg.Done()
		for range 100 {
			sess.ApplyFileActivity(time.Now())
			sess.SetCurrentTool("bash")
		}
	}()

	// Reader goroutine 1
	go func() {
		defer wg.Done()
		for range 100 {
			_ = sess.Snapshot()
		}
	}()

	// Reader goroutine 2
	go func() {
		defer wg.Done()
		for range 100 {
			_ = sess.GetStatus()
			_ = sess.Elapsed()
		}
	}()

	wg.Wait()
}

func TestEncodePathForDir(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/Users/pomesaka/github.com/pomesaka/sandbox", "-Users-pomesaka-github.com-pomesaka-sandbox"},
		{"/a/b/c", "-a-b-c"},
		{"/single", "-single"},
	}
	for _, tt := range tests {
		got := encodePathForDir(tt.input)
		if got != tt.want {
			t.Errorf("encodePathForDir(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDisplayChannel_String(t *testing.T) {
	tests := []struct {
		ch   DisplayChannel
		want string
	}{
		{DisplayJSONL, "jsonl"},
		{DisplayTmux, "tmux"},
		{DisplayChannel(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.ch.String(); got != tt.want {
			t.Errorf("DisplayChannel(%d).String() = %q, want %q", tt.ch, got, tt.want)
		}
	}
}

func TestDisplayChannel_Derivation(t *testing.T) {
	tests := []struct {
		status          Status
		wantDisplay     DisplayChannel
		wantProcessLive bool
	}{
		{StatusIdle, DisplayTmux, true},
		{StatusRunning, DisplayTmux, true},
		{StatusWaitingApproval, DisplayTmux, true},
		{StatusWaitingAnswer, DisplayTmux, true},
		{StatusSubagentRunning, DisplayTmux, true},
		{StatusCompleted, DisplayJSONL, false},
		{StatusError, DisplayJSONL, false},
		{StatusUnmanaged, DisplayJSONL, false},
	}
	for _, tt := range tests {
		t.Run(tt.status.ID(), func(t *testing.T) {
			s := NewSession("/tmp/repo", "repo")
			s.Status = tt.status
			if got := s.Snapshot().Display; got != tt.wantDisplay {
				t.Errorf("Display = %v, want %v", got, tt.wantDisplay)
			}
			if got := s.IsProcessAlive(); got != tt.wantProcessLive {
				t.Errorf("IsProcessAlive = %v, want %v", got, tt.wantProcessLive)
			}
		})
	}
}
