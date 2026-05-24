package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pomesaka/claude-deck/internal/ratelimits"
	"github.com/pomesaka/claude-deck/internal/usage"
)

func TestApplyRuntimeActivityFromJSONL_Codex(t *testing.T) {
	baseDir := t.TempDir()
	sessionID := "019e5353-bedb-7b62-8ce3-cbc4e1ca6c46"
	dir := filepath.Join(baseDir, "2026", "05", "23")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-05-23T14-34-17-"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(`{"timestamp":"2026-05-23T05:34:21.974Z","type":"event_msg","payload":{"type":"task_started"}}
{"timestamp":"2026-05-23T05:34:22.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call-1","arguments":"{\"cmd\":\"pwd\"}"}}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{usage: usage.NewCodexReader(baseDir)}
	sess := NewSession("/repo", "repo")
	sess.SessionChain = []RuntimeSessionID{RuntimeSessionID(sessionID)}
	sess.AttachProcess(os.Getpid())
	sess.SetStatus(StatusIdle)

	m.applyRuntimeActivityFromJSONL(sess, usage.FileEvent{SessionID: sessionID, Path: path})

	if got := sess.GetStatus(); got != StatusRunning {
		t.Fatalf("Status = %v, want %v", got, StatusRunning)
	}
	if got := sess.Snapshot().CurrentTool; got != "exec_command" {
		t.Fatalf("CurrentTool = %q, want exec_command", got)
	}

	if err := os.WriteFile(path, []byte(`{"timestamp":"2026-05-23T05:34:24.000Z","type":"event_msg","payload":{"type":"task_complete"}}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	m.applyRuntimeActivityFromJSONL(sess, usage.FileEvent{SessionID: sessionID, Path: path})

	if got := sess.GetStatus(); got != StatusIdle {
		t.Fatalf("Status = %v, want %v", got, StatusIdle)
	}
	if got := sess.Snapshot().CurrentTool; got != "" {
		t.Fatalf("CurrentTool = %q, want empty", got)
	}
}

func TestDiscoverExternalSessionsAdoptsRuntimeIDForManagedCodexWorkspace(t *testing.T) {
	baseDir := t.TempDir()
	sessionID := "019e5353-bedb-7b62-8ce3-cbc4e1ca6c46"
	workDir := "/repo/workspace"
	writeSessionCodexJSONL(t, baseDir, sessionID, workDir)

	sess := NewSession("/repo", "repo")
	sess.WorkspacePath = workDir

	m := &Manager{
		sessions: map[DeckSessionID]*Session{sess.ID: sess},
		usage:    usage.NewCodexReader(baseDir),
	}

	added, _ := m.DiscoverExternalSessions()
	if added != 0 {
		t.Fatalf("added = %d, want 0", added)
	}

	got := sess.ChainIDs()
	if len(got) != 1 || got[0] != RuntimeSessionID(sessionID) {
		t.Fatalf("SessionChain = %v, want [%s]", got, sessionID)
	}
	if len(m.sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1", len(m.sessions))
	}
}

func TestDiscoverExternalSessionsAdoptsRuntimeIDForKilledCodexWorkspace(t *testing.T) {
	baseDir := t.TempDir()
	mainRepo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(mainRepo, ".jj", "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "maika-650f")
	if err := os.MkdirAll(filepath.Join(workspace, ".jj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".jj", "repo"), []byte(filepath.Join(mainRepo, ".jj", "repo")), 0o644); err != nil {
		t.Fatal(err)
	}

	sessionID := "019e5a19-f293-7521-87b3-2126f990dbdd"
	writeSessionCodexJSONL(t, baseDir, sessionID, workspace)

	sess := NewSession(mainRepo, filepath.Base(mainRepo))
	sess.Name = filepath.Base(workspace)
	sess.WorkspacePath = ""
	sess.WorkspaceName = ""
	sess.SetStatus(StatusCompleted)

	m := &Manager{
		sessions: map[DeckSessionID]*Session{sess.ID: sess},
		usage:    usage.NewCodexReader(baseDir),
	}

	added, _ := m.DiscoverExternalSessions()
	if added != 0 {
		t.Fatalf("added = %d, want 0", added)
	}

	got := sess.ChainIDs()
	if len(got) != 1 || got[0] != RuntimeSessionID(sessionID) {
		t.Fatalf("SessionChain = %v, want [%s]", got, sessionID)
	}
	if len(m.sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1", len(m.sessions))
	}
}

func writeSessionCodexJSONL(t *testing.T, baseDir, sessionID, cwd string) {
	t.Helper()
	dir := filepath.Join(baseDir, "2026", "05", "24")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-05-24T22-08-30-"+sessionID+".jsonl")
	data := `{"timestamp":"2026-05-24T13:08:30.000Z","type":"session_meta","payload":{"id":"` + sessionID + `","timestamp":"2026-05-24T13:08:30.000Z","cwd":"` + cwd + `"}}
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRuntimeActivityFromJSONL_CodexRateLimits(t *testing.T) {
	baseDir := t.TempDir()
	dataDir := t.TempDir()
	sessionID := "019e5353-bedb-7b62-8ce3-cbc4e1ca6c46"
	dir := filepath.Join(baseDir, "2026", "05", "23")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-05-23T14-34-17-"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(`{"timestamp":"2026-05-23T05:34:21.974Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":16.0,"window_minutes":300,"resets_at":1779547690},"secondary":{"used_percent":17.0,"window_minutes":10080,"resets_at":1780114955}}}}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		usage:  usage.NewCodexReader(baseDir),
		config: ManagerConfig{DataDir: dataDir},
	}
	sess := NewSession("/repo", "repo")
	sess.SessionChain = []RuntimeSessionID{RuntimeSessionID(sessionID)}

	m.applyRuntimeActivityFromJSONL(sess, usage.FileEvent{SessionID: sessionID, Path: path})

	got := ratelimits.Load(dataDir)
	if !got.FiveHourAvailable || got.FiveHour.UsedPct != 16.0 || got.FiveHour.ResetsAt.Unix() != 1779547690 {
		t.Fatalf("FiveHour = %#v", got.FiveHour)
	}
	if !got.SevenDayAvailable || got.SevenDay.UsedPct != 17.0 || got.SevenDay.ResetsAt.Unix() != 1780114955 {
		t.Fatalf("SevenDay = %#v", got.SevenDay)
	}
}
