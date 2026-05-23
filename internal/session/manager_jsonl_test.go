package session

import (
	"os"
	"path/filepath"
	"testing"

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
