package session

import (
	"fmt"
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
		{StatusSubagentRunning, "サブエージェント実行中"},
		{StatusUnknown, "不明"},
		{Status(99), "不明"},
	}
	for _, tt := range tests {
		if got := tt.status.String(); got != tt.want {
			t.Errorf("Status(%d).String() = %q, want %q", tt.status, got, tt.want)
		}
	}
}

// The activities are live, Completed and Error are ended, and a status this
// version does not know is neither.
func TestStatus_IsLiveIsTerminal(t *testing.T) {
	tests := []struct {
		status       Status
		live, ending bool
	}{
		{StatusUnknown, false, false},
		{StatusRunning, true, false},
		{StatusWaitingApproval, true, false},
		{StatusWaitingAnswer, true, false},
		{StatusIdle, true, false},
		{StatusSubagentRunning, true, false},
		{StatusCompleted, false, true},
		{StatusError, false, true},
	}
	for _, tt := range tests {
		if got := tt.status.IsLive(); got != tt.live {
			t.Errorf("%s.IsLive() = %v, want %v", tt.status.ID(), got, tt.live)
		}
		if got := tt.status.IsTerminal(); got != tt.ending {
			t.Errorf("%s.IsTerminal() = %v, want %v", tt.status.ID(), got, tt.ending)
		}
	}
}

// The store holds Status.ID strings; parsing must round-trip them and turn
// anything else into Unknown.
func TestStatusFromID(t *testing.T) {
	for _, s := range []Status{StatusRunning, StatusWaitingApproval, StatusWaitingAnswer, StatusIdle, StatusSubagentRunning, StatusCompleted, StatusError} {
		if got, ok := StatusFromID(s.ID()); !ok || got != s {
			t.Errorf("StatusFromID(%q) = %v, %v; want %v, true", s.ID(), got, ok, s)
		}
	}
	for _, id := range []string{"unmanaged", "", "bogus", "unknown"} {
		if got, ok := StatusFromID(id); ok || got != StatusUnknown {
			t.Errorf("StatusFromID(%q) = %v, %v; want Unknown, false", id, got, ok)
		}
	}
}

// A hook may report only the activities; each maps to a distinct live status
// with the same ID.
func TestActivityFromID(t *testing.T) {
	seen := map[Status]bool{}
	for _, id := range []string{"running", "idle", "waiting_approval", "waiting_answer", "subagent_running"} {
		a, ok := ActivityFromID(id)
		if !ok {
			t.Errorf("ActivityFromID(%q) not accepted", id)
			continue
		}
		if a.Status().ID() != id || !a.Status().IsLive() {
			t.Errorf("ActivityFromID(%q).Status() = %v", id, a.Status())
		}
		if seen[a.Status()] {
			t.Errorf("two activities map to %v", a.Status())
		}
		seen[a.Status()] = true
	}
	for _, id := range []string{"completed", "error", "unmanaged", "", "bogus"} {
		if _, ok := ActivityFromID(id); ok {
			t.Errorf("ActivityFromID(%q) accepted, want refused", id)
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
		{StatusUnknown, false},
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

// claude-deck has a process only for a deck session doing something; only that
// one goes on tmux. r starts an external session (adopting it) or a finished one.
func TestSnapshot_ProcessAndResume(t *testing.T) {
	tests := []struct {
		origin      Origin
		status      Status
		wantProcess bool
		wantResume  bool
	}{
		{OriginDeck, StatusIdle, true, false},
		{OriginDeck, StatusRunning, true, false},
		{OriginDeck, StatusWaitingApproval, true, false},
		{OriginDeck, StatusWaitingAnswer, true, false},
		{OriginDeck, StatusSubagentRunning, true, false},
		{OriginDeck, StatusCompleted, false, true},
		{OriginDeck, StatusError, false, true},
		{OriginDeck, StatusUnknown, false, false},
		{OriginExternal, StatusUnknown, false, true},
		// Does not happen (external sessions have no status), but must not count as a process.
		{OriginExternal, StatusRunning, false, true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d/%s", tt.origin, tt.status.ID()), func(t *testing.T) {
			s := NewSession("/tmp/repo", "repo")
			s.Origin, s.Status = tt.origin, tt.status
			snap := s.Snapshot()
			if got := snap.HasProcess(); got != tt.wantProcess {
				t.Errorf("HasProcess = %v, want %v", got, tt.wantProcess)
			}
			wantDisplay := DisplayJSONL
			if tt.wantProcess {
				wantDisplay = DisplayTmux
			}
			if snap.Display != wantDisplay {
				t.Errorf("Display = %v, want %v", snap.Display, wantDisplay)
			}
			if got := s.IsProcessAlive(); got != tt.wantProcess {
				t.Errorf("IsProcessAlive = %v, want %v", got, tt.wantProcess)
			}
			if got := snap.CanResume(); got != tt.wantResume {
				t.Errorf("CanResume = %v, want %v", got, tt.wantResume)
			}
		})
	}
}

// Sessions and store rows are ordered by the latest of the three times. The
// store's LastActivity is written only when the TUI exits cleanly, so it must
// not hide a later FinishedAt.
func TestSortTimeOf(t *testing.T) {
	at := func(sec int64) time.Time { return time.Unix(1_700_000_000+sec, 0) }
	tests := []struct {
		name         string
		lastActivity time.Time
		finishedAt   *time.Time
		startedAt    time.Time
		want         time.Time
	}{
		{name: "only StartedAt", startedAt: at(1), want: at(1)},
		{name: "FinishedAt after a stale LastActivity", lastActivity: at(2), finishedAt: new(at(5)), startedAt: at(1), want: at(5)},
		{name: "LastActivity is the latest", lastActivity: at(9), finishedAt: new(at(5)), startedAt: at(1), want: at(9)},
		{name: "StartedAt is the latest", lastActivity: at(2), finishedAt: new(at(3)), startedAt: at(4), want: at(4)},
		{name: "nothing known", want: time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sortTimeOf(tt.lastActivity, tt.finishedAt, tt.startedAt); !got.Equal(tt.want) {
				t.Errorf("sortTimeOf() = %v, want %v", got, tt.want)
			}
		})
	}
}
