package usage

import (
	"path/filepath"
	"testing"
)

func setupCodexReader(t *testing.T) (*Reader, string) {
	t.Helper()
	baseDir := t.TempDir()
	return NewCodexReader(baseDir), baseDir
}

func writeCodexJSONL(t *testing.T, baseDir, sessionID, content string) string {
	t.Helper()
	dir := filepath.Join(baseDir, "2026", "05", "23")
	path := filepath.Join(dir, "rollout-2026-05-23T14-34-17-"+sessionID+".jsonl")
	writeJSONL(t, dir, filepath.Base(path), content)
	return path
}

func TestCodexReadSessionInfoByID(t *testing.T) {
	r, baseDir := setupCodexReader(t)
	sessionID := "019e5353-bedb-7b62-8ce3-cbc4e1ca6c46"
	jsonl := `{"timestamp":"2026-05-23T05:34:21.970Z","type":"session_meta","payload":{"id":"019e5353-bedb-7b62-8ce3-cbc4e1ca6c46","timestamp":"2026-05-23T05:34:17.850Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.133.0","model_provider":"openai"}}
{"timestamp":"2026-05-23T05:34:21.974Z","type":"turn_context","payload":{"turn_id":"turn-1","cwd":"/repo","approval_policy":"on-request","model":"gpt-5.5"}}
{"timestamp":"2026-05-23T05:34:21.974Z","type":"event_msg","payload":{"type":"user_message","message":"implement codex support"}}
{"timestamp":"2026-05-23T05:34:22.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":5},"last_token_usage":{"input_tokens":100,"output_tokens":50},"model_context_window":258400}}}
`
	writeCodexJSONL(t, baseDir, sessionID, jsonl)

	info := r.ReadSessionInfoByID(sessionID)
	if info == nil {
		t.Fatal("expected non-nil info")
	}
	if info.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", info.SessionID, sessionID)
	}
	if info.CWD != "/repo" {
		t.Errorf("CWD = %q, want /repo", info.CWD)
	}
	if info.Model != "gpt-5.5" {
		t.Errorf("Model = %q, want gpt-5.5", info.Model)
	}
	if info.PermissionMode != "on-request" {
		t.Errorf("PermissionMode = %q, want on-request", info.PermissionMode)
	}
	if info.Prompt != "implement codex support" {
		t.Errorf("Prompt = %q", info.Prompt)
	}
	if info.Tokens.InputTokens != 100 || info.Tokens.OutputTokens != 50 {
		t.Errorf("Tokens = %+v, want input=100 output=50", info.Tokens)
	}
}

func TestCodexListAllSessions(t *testing.T) {
	r, baseDir := setupCodexReader(t)
	writeCodexJSONL(t, baseDir, "019e5353-bedb-7b62-8ce3-cbc4e1ca6c46", `{"timestamp":"2026-05-23T05:34:21.970Z","type":"session_meta","payload":{"id":"019e5353-bedb-7b62-8ce3-cbc4e1ca6c46","cwd":"/repo-a"}}
`)
	writeCodexJSONL(t, baseDir, "019e5354-bedb-7b62-8ce3-cbc4e1ca6c47", `{"timestamp":"2026-05-23T05:34:21.970Z","type":"session_meta","payload":{"id":"019e5354-bedb-7b62-8ce3-cbc4e1ca6c47","cwd":"/repo-b"}}
`)

	results := r.ListAllSessions(0, 0, 0)
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
}

func TestCodexLogStreamer(t *testing.T) {
	_, baseDir := setupCodexReader(t)
	path := writeCodexJSONL(t, baseDir, "019e5353-bedb-7b62-8ce3-cbc4e1ca6c46", `{"timestamp":"2026-05-23T05:34:21.974Z","type":"event_msg","payload":{"type":"user_message","message":"hello\nmore"}}
{"timestamp":"2026-05-23T05:34:22.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call-1","arguments":"{\"cmd\":\"pwd\"}"}}
{"timestamp":"2026-05-23T05:34:23.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"ok"}}
{"timestamp":"2026-05-23T05:34:24.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}
`)

	s := NewCodexLogStreamer(path)
	s.ReadAll()
	entries := s.Entries()
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3: %#v", len(entries), entries)
	}
	if entries[0].Kind != LogEntryUser || entries[0].Text != "hello" {
		t.Errorf("entries[0] = %#v", entries[0])
	}
	if entries[1].Kind != LogEntryToolUse || entries[1].ToolName != "exec_command" || !entries[1].HasResult {
		t.Errorf("entries[1] = %#v", entries[1])
	}
	if entries[2].Kind != LogEntryText || entries[2].Text != "done" {
		t.Errorf("entries[2] = %#v", entries[2])
	}
}

func TestCodexRuntimeActivity(t *testing.T) {
	r, baseDir := setupCodexReader(t)
	sessionID := "019e5353-bedb-7b62-8ce3-cbc4e1ca6c46"
	path := writeCodexJSONL(t, baseDir, sessionID, `{"timestamp":"2026-05-23T05:34:21.974Z","type":"event_msg","payload":{"type":"task_started"}}
{"timestamp":"2026-05-23T05:34:22.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call-1","arguments":"{\"cmd\":\"pwd\"}"}}
`)

	got := r.ReadRuntimeActivity(path)
	if got.SessionID != sessionID {
		t.Fatalf("SessionID = %q, want %q", got.SessionID, sessionID)
	}
	if got.Kind != RuntimeActivityRunning {
		t.Fatalf("Kind = %v, want RuntimeActivityRunning", got.Kind)
	}
	if got.CurrentTool != "exec_command" {
		t.Fatalf("CurrentTool = %q, want exec_command", got.CurrentTool)
	}

	path = writeCodexJSONL(t, baseDir, sessionID, `{"timestamp":"2026-05-23T05:34:21.974Z","type":"event_msg","payload":{"type":"task_started"}}
{"timestamp":"2026-05-23T05:34:24.000Z","type":"event_msg","payload":{"type":"task_complete"}}
`)

	got = r.ReadRuntimeActivity(path)
	if got.Kind != RuntimeActivityIdle {
		t.Fatalf("Kind = %v, want RuntimeActivityIdle", got.Kind)
	}
	if !got.ClearTool {
		t.Fatal("ClearTool = false, want true")
	}
}

func TestCodexRuntimeActivityRateLimits(t *testing.T) {
	r, baseDir := setupCodexReader(t)
	sessionID := "019e5353-bedb-7b62-8ce3-cbc4e1ca6c46"
	path := writeCodexJSONL(t, baseDir, sessionID, `{"timestamp":"2026-05-23T05:34:21.974Z","type":"event_msg","payload":{"type":"task_started"}}
{"timestamp":"2026-05-23T05:34:22.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":16.0,"window_minutes":300,"resets_at":1779547690},"secondary":{"used_percent":17.0,"window_minutes":10080,"resets_at":1780114955}}}}
`)

	got := r.ReadRuntimeActivity(path)
	if got.Kind != RuntimeActivityRunning {
		t.Fatalf("Kind = %v, want RuntimeActivityRunning", got.Kind)
	}
	if got.RateLimits == nil {
		t.Fatal("RateLimits = nil, want limits")
	}
	if !got.RateLimits.FiveHourAvailable || got.RateLimits.FiveHour.UsedPct != 16.0 || got.RateLimits.FiveHour.ResetsAt != 1779547690 {
		t.Fatalf("FiveHour = %#v", got.RateLimits.FiveHour)
	}
	if !got.RateLimits.SevenDayAvailable || got.RateLimits.SevenDay.UsedPct != 17.0 || got.RateLimits.SevenDay.ResetsAt != 1780114955 {
		t.Fatalf("SevenDay = %#v", got.RateLimits.SevenDay)
	}
}
