package session

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/pomesaka/claude-deck/internal/agentruntime"
	"github.com/pomesaka/claude-deck/internal/ratelimits"
	"github.com/pomesaka/claude-deck/internal/store"
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

	m := newCodexTestManager(t, baseDir)
	id := createPlainSession(t, m).ID
	if err := RecordSessionStart(m.store, id, RuntimeSessionID(sessionID), SourceStartup); err != nil {
		t.Fatal(err)
	}
	m.Reload()
	sess := m.GetSession(id)

	m.applyRuntimeActivityFromJSONL(sess, usage.FileEvent{SessionID: sessionID, Path: path})

	if got := mustGet(t, m.store, id).Status; got != StatusRunning.ID() {
		t.Fatalf("store status = %q, want %q", got, StatusRunning.ID())
	}
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

	if got := mustGet(t, m.store, id).Status; got != StatusIdle.ID() {
		t.Fatalf("store status = %q, want %q", got, StatusIdle.ID())
	}
	if got := sess.Snapshot().CurrentTool; got != "" {
		t.Fatalf("CurrentTool = %q, want empty", got)
	}
}

func newCodexTestManager(t *testing.T, transcriptDir string) *Manager {
	t.Helper()
	m, _ := newTestManager(t)
	m.usage = usage.NewCodexReader(transcriptDir)
	m.config.AgentRuntime = agentruntime.CodexRuntime{Command: "codex"}
	return m
}

func TestDiscoverExternalSessionsAdoptsRuntimeIDForManagedCodexWorkspace(t *testing.T) {
	baseDir := t.TempDir()
	sessionID := "019e5353-bedb-7b62-8ce3-cbc4e1ca6c46"

	m := newCodexTestManager(t, baseDir)
	sess := createPlainSession(t, m)
	writeSessionCodexJSONL(t, baseDir, sessionID, sess.Snapshot().WorkspacePath)

	added, _ := m.DiscoverExternalSessions()
	if added != 0 {
		t.Fatalf("added = %d, want 0", added)
	}
	if got := mustGet(t, m.store, sess.ID).SessionChain; !slices.Equal(got, []string{sessionID}) {
		t.Fatalf("store SessionChain = %v, want [%s]", got, sessionID)
	}
	if got := m.GetSession(sess.ID).ChainIDs(); !slices.Equal(got, []RuntimeSessionID{RuntimeSessionID(sessionID)}) {
		t.Fatalf("SessionChain = %v, want [%s]", got, sessionID)
	}
	if n := len(m.copySessionsList()); n != 1 {
		t.Fatalf("len(sessions) = %d, want 1", n)
	}
}

func TestDiscoverExternalSessions_ClaudeDoesNotAdoptByWorkspace(t *testing.T) {
	baseDir := t.TempDir()
	sessionID := "019e5353-bedb-7b62-8ce3-cbc4e1ca6c46"

	m := newCodexTestManager(t, baseDir)
	m.config.AgentRuntime = nil
	sess := createPlainSession(t, m)
	writeSessionCodexJSONL(t, baseDir, sessionID, sess.Snapshot().WorkspacePath)

	m.DiscoverExternalSessions()
	if got := mustGet(t, m.store, sess.ID).SessionChain; len(got) != 0 {
		t.Fatalf("store SessionChain = %v, want empty", got)
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

	m := newCodexTestManager(t, baseDir)
	rec := store.Record{
		ID:       string(GenerateSessionID()),
		Name:     filepath.Base(workspace),
		RepoPath: mainRepo,
		RepoName: filepath.Base(mainRepo),
		Status:   StatusCompleted.ID(),
	}
	if err := m.store.Insert(rec); err != nil {
		t.Fatal(err)
	}
	m.Reload()

	added, _ := m.DiscoverExternalSessions()
	if added != 0 {
		t.Fatalf("added = %d, want 0", added)
	}
	if got := mustGet(t, m.store, DeckSessionID(rec.ID)).SessionChain; !slices.Equal(got, []string{sessionID}) {
		t.Fatalf("store SessionChain = %v, want [%s]", got, sessionID)
	}
	if n := len(m.copySessionsList()); n != 1 {
		t.Fatalf("len(sessions) = %d, want 1", n)
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
