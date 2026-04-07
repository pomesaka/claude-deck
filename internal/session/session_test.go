package session

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pomesaka/claude-deck/internal/store"
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
	}
	for _, tt := range tests {
		if got := tt.status.IsTerminal(); got != tt.want {
			t.Errorf("Status(%d).IsTerminal() = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestStatus_CanTransitionTo(t *testing.T) {
	tests := []struct {
		from, to Status
		want     bool
	}{
		// Identity transitions
		{StatusIdle, StatusIdle, true},
		{StatusRunning, StatusRunning, true},

		// Idle transitions
		{StatusIdle, StatusRunning, true},
		{StatusIdle, StatusCompleted, true},
		{StatusIdle, StatusError, true},
		{StatusIdle, StatusWaitingApproval, false},

		// Running transitions
		{StatusRunning, StatusIdle, true},
		{StatusRunning, StatusWaitingApproval, true},
		{StatusRunning, StatusWaitingAnswer, true},
		{StatusRunning, StatusCompleted, true},
		{StatusRunning, StatusError, true},

		// WaitingApproval transitions
		{StatusWaitingApproval, StatusRunning, true},
		{StatusWaitingApproval, StatusIdle, true},
		{StatusWaitingApproval, StatusCompleted, true},
		{StatusWaitingApproval, StatusWaitingAnswer, false},

		// WaitingAnswer transitions
		{StatusWaitingAnswer, StatusRunning, true},
		{StatusWaitingAnswer, StatusIdle, true},
		{StatusWaitingAnswer, StatusCompleted, true},
		{StatusWaitingAnswer, StatusWaitingApproval, false},

		// Terminal state transitions
		{StatusCompleted, StatusIdle, true},   // Resume
		{StatusCompleted, StatusRunning, false},
		{StatusCompleted, StatusError, true},
		{StatusError, StatusIdle, true},        // Resume
		{StatusError, StatusRunning, false},

		// Unmanaged never transitions
		{StatusUnmanaged, StatusRunning, false},
		{StatusUnmanaged, StatusCompleted, false},
	}
	for _, tt := range tests {
		got := tt.from.canTransitionTo(tt.to)
		if got != tt.want {
			t.Errorf("Status(%v).canTransitionTo(%v) = %v, want %v", tt.from, tt.to, got, tt.want)
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
	}
	for _, tt := range tests {
		if got := tt.status.NeedsAttention(); got != tt.want {
			t.Errorf("Status(%d).NeedsAttention() = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestSessionPhase_String(t *testing.T) {
	tests := []struct {
		phase SessionPhase
		want  string
	}{
		{PhaseActive, "Active"},
		{PhaseArchived, "Archived"},
		{PhaseExternal, "External"},
		{SessionPhase(99), "Unknown"},
	}
	for _, tt := range tests {
		if got := tt.phase.String(); got != tt.want {
			t.Errorf("SessionPhase(%d).String() = %q, want %q", tt.phase, got, tt.want)
		}
	}
}

func TestSession_Phase(t *testing.T) {
	tests := []struct {
		name    string
		status  Status
		managed bool
		want    SessionPhase
	}{
		{"running managed", StatusRunning, true, PhaseActive},
		{"idle managed", StatusIdle, true, PhaseActive},
		{"waiting managed", StatusWaitingApproval, true, PhaseActive},
		{"idle unmanaged", StatusIdle, false, PhaseActive},       // Idle but not terminal
		{"completed unmanaged", StatusCompleted, false, PhaseArchived},
		{"error unmanaged", StatusError, false, PhaseArchived},
		{"completed still managed", StatusCompleted, true, PhaseActive}, // rare: watchProcess hasn't run yet
		{"unmanaged", StatusUnmanaged, false, PhaseExternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := NewSession("/repo", "repo")
			sess.Status = tt.status
			sess.managed = tt.managed
			snap := sess.Snapshot()
			if snap.Phase != tt.want {
				t.Errorf("Phase = %v, want %v", snap.Phase, tt.want)
			}
		})
	}
}

func TestTokenUsage_TotalTokens(t *testing.T) {
	tu := TokenUsage{InputTokens: 100, OutputTokens: 50}
	if got := tu.TotalTokens(); got != 150 {
		t.Errorf("TotalTokens() = %d, want 150", got)
	}
}

func TestTokenUsage_EstimateCost(t *testing.T) {
	pricing := PricingPolicy{
		InputPerMTok:      15.0,
		OutputPerMTok:     75.0,
		CacheWritePerMTok: 18.75,
		CacheReadPerMTok:  1.50,
	}

	tu := TokenUsage{
		InputTokens:              1_000_000,
		OutputTokens:             500_000,
		CacheCreationInputTokens: 200_000,
		CacheReadInputTokens:     300_000,
	}

	cost := tu.EstimateCost(pricing)
	// 1M * 15/1M + 0.5M * 75/1M + 0.2M * 18.75/1M + 0.3M * 1.5/1M
	// = 15.0 + 37.5 + 3.75 + 0.45 = 56.7
	expected := 56.7
	if cost < expected-0.01 || cost > expected+0.01 {
		t.Errorf("EstimateCost = %f, want ~%f", cost, expected)
	}

	// Zero pricing returns 0
	zeroCost := tu.EstimateCost(PricingPolicy{})
	if zeroCost != 0 {
		t.Errorf("EstimateCost with zero pricing = %f, want 0", zeroCost)
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

func TestSession_SetGetStatus(t *testing.T) {
	sess := NewSession("/repo", "repo")

	sess.SetStatus(StatusWaitingApproval)
	if got := sess.GetStatus(); got != StatusWaitingApproval {
		t.Errorf("GetStatus() = %v, want StatusWaitingApproval", got)
	}

	sess.SetStatus(StatusCompleted)
	if got := sess.GetStatus(); got != StatusCompleted {
		t.Errorf("GetStatus() = %v, want StatusCompleted", got)
	}
	if sess.FinishedAt == nil {
		t.Error("expected FinishedAt to be set on completion")
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

func TestSession_AddTokens(t *testing.T) {
	sess := NewSession("/repo", "repo")
	sess.AddTokens(100, 50)
	sess.AddTokens(200, 100)

	snap := sess.Snapshot()
	if snap.TokenUsage.InputTokens != 300 {
		t.Errorf("InputTokens = %d, want 300", snap.TokenUsage.InputTokens)
	}
	if snap.TokenUsage.OutputTokens != 150 {
		t.Errorf("OutputTokens = %d, want 150", snap.TokenUsage.OutputTokens)
	}
}

func TestSession_AppendLog(t *testing.T) {
	sess := NewSession("/repo", "repo")
	sess.AppendLog("line1")
	sess.AppendLog("line2")

	logs := sess.GetLogs()
	if len(logs) != 2 {
		t.Fatalf("expected 2 log lines, got %d", len(logs))
	}
	if logs[0] != "line1" || logs[1] != "line2" {
		t.Errorf("logs = %v, want [line1, line2]", logs)
	}
}

func TestSession_AppendLog_Truncation(t *testing.T) {
	sess := NewSession("/repo", "repo")
	for i := range 1100 {
		sess.AppendLog(strings.Repeat("x", i%10))
	}
	logs := sess.GetLogs()
	if len(logs) != 1000 {
		t.Errorf("expected 1000 log lines after truncation, got %d", len(logs))
	}
}

func TestSession_GetPTYDisplayLines(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  []string
	}{
		{
			name:  "plain lines with \\r\\n endings",
			lines: []string{"hello\r", "world\r"},
			want:  []string{"hello", "world"},
		},
		{
			name:  "carriage return overwrites line",
			lines: []string{"old text\rnew text"},
			want:  []string{"new text"},
		},
		{
			name:  "ANSI sequences preserved via Render",
			lines: []string{"\x1b[32mgreen\x1b[0m\r", "\x1b[?25lhidden cursor\r"},
			want:  []string{"\x1b[32mgreen\x1b[m", "hidden cursor"},
		},
		{
			name:  "progress bar overwrite",
			lines: []string{"Downloading 50%\rDownloading 100%\r"},
			want:  []string{"Downloading 100%"},
		},
		{
			name:  "cursor movement stripped but colors preserved",
			lines: []string{"\x1b[H\x1b[2Jscreen content\r"},
			want:  []string{"screen content"},
		},
		{
			name:  "empty input",
			lines: nil,
			want:  nil,
		},
		{
			name:  "cursor positioning preserves spacing",
			lines: []string{"\x1b[1;1HDo you want to proceed?\r"},
			want:  []string{"Do you want to proceed?"},
		},
		{
			name:  "cursor save and restore",
			lines: []string{"header\r", "\x1b7\x1b[3;1Hinserted line\x1b8continued\r"},
			want:  []string{"header", "continued", "inserted line"},
		},
		{
			name:  "cursor column positioning",
			lines: []string{"A\x1b[10GB\r"},
			want:  []string{"A        B"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := NewSession("/repo", "repo")
			sess.InitDisplay(120, 40, 0)
			for _, line := range tt.lines {
				sess.AppendLog(line)
			}
			got := sess.GetPTYDisplayLines()
			if len(got) != len(tt.want) {
				t.Fatalf("got %d lines %v, want %d lines %v", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("line[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
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
	sess.AddTokens(500, 200)

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
	if snap.TokenUsage.InputTokens != 500 {
		t.Errorf("snapshot InputTokens = %d", snap.TokenUsage.InputTokens)
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
			sess.AddTokens(1, 1)
			sess.SetCurrentTool("bash")
			sess.AppendLog("line")
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
			_ = sess.GetLogs()
			_ = sess.Elapsed()
		}
	}()

	wg.Wait()
}

func TestSession_SetStatus_FromHook(t *testing.T) {
	sess := NewSession("/repo", "repo")

	// Hook 経由のステータス更新も SetStatus を使う
	sess.SetStatus(StatusWaitingApproval)
	if got := sess.GetStatus(); got != StatusWaitingApproval {
		t.Errorf("GetStatus() = %v, want StatusWaitingApproval", got)
	}

	sess.SetStatus(StatusIdle)
	if got := sess.GetStatus(); got != StatusIdle {
		t.Errorf("GetStatus() = %v, want StatusIdle", got)
	}

	// Completed 経由で FinishedAt が設定されることを確認
	sess.SetStatus(StatusCompleted)
	if got := sess.GetStatus(); got != StatusCompleted {
		t.Errorf("GetStatus() = %v, want StatusCompleted", got)
	}
	sess.mu.RLock()
	if sess.FinishedAt == nil {
		t.Error("FinishedAt should be set after StatusCompleted")
	}
	sess.mu.RUnlock()
}

func TestContainsBrailleSpinner(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"⠐ thinking...", true},
		{"  \x1b[33m⠹\x1b[0m go test ./...", true},
		{"normal text", false},
		{"", false},
		{"✳ Claude Code", false}, // Dingbat, not Braille
	}
	for _, tt := range tests {
		if got := containsBrailleSpinner(tt.line); got != tt.want {
			t.Errorf("containsBrailleSpinner(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
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

func TestLoadExisting_DeletedDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := store.New(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	// 存在しないパスを WorkspacePath に持つ完了済みセッション
	deletedPath := filepath.Join(tmpDir, "nonexistent-workspace")
	finishedAt := time.Now().Add(-1 * time.Hour)
	sess := &Session{
		ID:            "test-deleted-dir",
		Name:          "test",
		RepoPath:      "/repo",
		WorkspacePath: deletedPath,
		Status:        StatusCompleted,
		FinishedAt:    &finishedAt,
	}
	if err := st.Save(string(sess.ID), sess); err != nil {
		t.Fatal(err)
	}

	// 存在するパスを持つ完了済みセッション（こちらはエラーにならない）
	existingPath := t.TempDir()
	sess2 := &Session{
		ID:            "test-existing-dir",
		Name:          "test2",
		RepoPath:      "/repo",
		WorkspacePath: existingPath,
		Status:        StatusCompleted,
		FinishedAt:    &finishedAt,
	}
	if err := st.Save(string(sess2.ID), sess2); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(context.Background(), st, ManagerConfig{
		DataDir:     tmpDir,
		MaxSessions: 30,
	})
	if err := mgr.LoadExisting(); err != nil {
		t.Fatal(err)
	}

	// 削除済みディレクトリのセッションは StatusError になる
	got := mgr.GetSession("test-deleted-dir")
	if got == nil {
		t.Fatal("session test-deleted-dir not found")
	}
	if got.Status != StatusError {
		t.Errorf("deleted dir session status = %v, want StatusError", got.Status)
	}
	if got.ErrorMessage == "" {
		t.Error("expected non-empty ErrorMessage for deleted directory")
	}
	if !strings.Contains(got.ErrorMessage, deletedPath) {
		t.Errorf("ErrorMessage = %q, want to contain %q", got.ErrorMessage, deletedPath)
	}
	// FinishedAt は元の値を保持（上書きしない）
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finishedAt) {
		t.Errorf("FinishedAt should preserve original value, got %v", got.FinishedAt)
	}

	// 存在するディレクトリのセッションは StatusCompleted のまま
	got2 := mgr.GetSession("test-existing-dir")
	if got2 == nil {
		t.Fatal("session test-existing-dir not found")
	}
	if got2.Status != StatusCompleted {
		t.Errorf("existing dir session status = %v, want StatusCompleted", got2.Status)
	}
}

func TestHostingMode_String(t *testing.T) {
	tests := []struct {
		mode HostingMode
		want string
	}{
		{HostEmbedded, "Embedded"},
		{HostExternal, "External"},
		{HostingMode(99), "Unknown"},
	}
	for _, tt := range tests {
		if got := tt.mode.String(); got != tt.want {
			t.Errorf("HostingMode(%d).String() = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

func TestDisplayChannel_String(t *testing.T) {
	tests := []struct {
		ch   DisplayChannel
		want string
	}{
		{DisplayPTY, "PTY"},
		{DisplayJSONL, "JSONL"},
		{DisplayNone, "None"},
		{DisplayChannel(99), "Unknown"},
	}
	for _, tt := range tests {
		if got := tt.ch.String(); got != tt.want {
			t.Errorf("DisplayChannel(%d).String() = %q, want %q", tt.ch, got, tt.want)
		}
	}
}

func TestDisplayChannel_Derivation(t *testing.T) {
	tests := []struct {
		name    string
		hosting HostingMode
		managed bool
		want    DisplayChannel
	}{
		{"embedded+managed", HostEmbedded, true, DisplayPTY},
		{"embedded+unmanaged", HostEmbedded, false, DisplayJSONL},
		{"external+managed", HostExternal, true, DisplayNone},
		{"external+unmanaged", HostExternal, false, DisplayJSONL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSession("/tmp/repo", "repo")
			s.Hosting = tt.hosting
			s.managed = tt.managed
			snap := s.Snapshot()
			if snap.Display != tt.want {
				t.Errorf("Display = %v, want %v", snap.Display, tt.want)
			}
		})
	}
}

func TestNewExternalSession(t *testing.T) {
	s := NewExternalSession("/tmp/repo", "repo")
	if s.Hosting != HostExternal {
		t.Errorf("Hosting = %v, want HostExternal", s.Hosting)
	}
	if s.display != nil {
		t.Error("external session should not have a PTYDisplay")
	}
	// AppendRaw should not panic with nil display
	s.AppendRaw([]byte("hello\n"))
	logs := s.GetLogs()
	if len(logs) == 0 {
		t.Error("LogLines should still be populated for external sessions")
	}
}

func TestNewSession_DefaultHosting(t *testing.T) {
	s := NewSession("/tmp/repo", "repo")
	if s.Hosting != HostEmbedded {
		t.Errorf("Hosting = %v, want HostEmbedded", s.Hosting)
	}
	// PTYDisplay is nil until InitDisplay is called by Manager.
	// This avoids double-creation with wrong dimensions.
	if s.display != nil {
		t.Error("new session should not have display until InitDisplay is called")
	}
	// InitDisplay and verify
	s.InitDisplay(80, 24, 0)
	if s.display == nil {
		t.Error("display should be non-nil after InitDisplay")
	}
}
