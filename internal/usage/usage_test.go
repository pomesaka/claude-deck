package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setupTestReader(t *testing.T) (*Reader, string) {
	t.Helper()
	baseDir := t.TempDir()
	return NewReader(baseDir), baseDir
}

func writeJSONL(t *testing.T, dir, filename, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNewReader_DefaultDir(t *testing.T) {
	r := NewReader("")
	if r.baseDir == "" {
		t.Error("expected non-empty baseDir")
	}
}

func TestReadSessionInfoByID(t *testing.T) {
	r, baseDir := setupTestReader(t)
	projDir := filepath.Join(baseDir, "project1")

	jsonl := `{"type":"user","sessionId":"sess-002","cwd":"/home/user/myrepo","timestamp":"2026-02-26T10:00:00Z","permissionMode":"plan","gitBranch":"main","message":{"role":"user","content":"implement feature X"}}
{"type":"assistant","sessionId":"sess-002","timestamp":"2026-02-26T10:01:00Z","message":{"role":"assistant","model":"claude-opus-4-6","usage":{"input_tokens":500,"output_tokens":200,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
`
	writeJSONL(t, projDir, "sess-002.jsonl", jsonl)

	info := r.ReadSessionInfoByID("sess-002")
	if info == nil {
		t.Fatal("expected non-nil info")
	}
	if info.SessionID != "sess-002" {
		t.Errorf("SessionID = %q", info.SessionID)
	}
	if info.CWD != "/home/user/myrepo" {
		t.Errorf("CWD = %q", info.CWD)
	}
	if info.Prompt != "implement feature X" {
		t.Errorf("Prompt = %q", info.Prompt)
	}
	if info.PermissionMode != "plan" {
		t.Errorf("PermissionMode = %q", info.PermissionMode)
	}
	if info.GitBranch != "main" {
		t.Errorf("GitBranch = %q", info.GitBranch)
	}
	if info.StartedAt.IsZero() {
		t.Error("expected non-zero StartedAt")
	}
	if info.LastActivity.IsZero() {
		t.Error("expected non-zero LastActivity")
	}
}

// toolUseResult is a string for some tools. Reading must neither stop at such a
// line nor lose what follows it.
func TestReadSessionInfoByID_ToolUseResultShapes(t *testing.T) {
	const (
		user      = `{"type":"user","cwd":"/repo","timestamp":"2026-02-26T10:00:00Z","message":{"role":"user","content":"go"}}`
		assistant = `{"type":"assistant","timestamp":"2026-02-26T10:00:02Z","message":{"role":"assistant","model":"m","usage":{"input_tokens":7,"output_tokens":3}}}`
	)
	tests := []struct {
		name   string
		result string
	}{
		{"string", `{"type":"user","timestamp":"2026-02-26T10:00:01Z","toolUseResult":"Error: denied","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1"}]}}`},
		{"object", `{"type":"user","timestamp":"2026-02-26T10:00:01Z","toolUseResult":{"filePath":"a.go","structuredPatch":[]},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1"}]}}`},
		{"array", `{"type":"user","timestamp":"2026-02-26T10:00:01Z","toolUseResult":[{"type":"text","text":"ok"}],"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1"}]}}`},
		{"malformed line", `{"type":"user","toolUseResult":`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, baseDir := setupTestReader(t)
			writeJSONL(t, filepath.Join(baseDir, "p"), "s.jsonl", user+"\n"+tt.result+"\n"+assistant+"\n")

			info := r.ReadSessionInfoByID("s")
			if info == nil {
				t.Fatal("expected non-nil info")
			}
			// The assistant line after the tool result carries the latest timestamp.
			if want := time.Date(2026, 2, 26, 10, 0, 2, 0, time.UTC); !info.LastActivity.Equal(want) {
				t.Errorf("LastActivity = %v, want %v (the line after the tool result)", info.LastActivity, want)
			}
		})
	}
}

func TestListAllSessions(t *testing.T) {
	r, baseDir := setupTestReader(t)
	projDir := filepath.Join(baseDir, "project1")

	jsonl1 := `{"type":"user","sessionId":"sess-a","cwd":"/repo1","timestamp":"2026-02-26T10:00:00Z","message":{"role":"user","content":"task a"}}
`
	jsonl2 := `{"type":"user","sessionId":"sess-b","cwd":"/repo2","timestamp":"2026-02-26T11:00:00Z","message":{"role":"user","content":"task b"}}
`
	writeJSONL(t, projDir, "sess-a.jsonl", jsonl1)
	writeJSONL(t, projDir, "sess-b.jsonl", jsonl2)

	results := r.ListAllSessions(0, 0, 0)
	if len(results) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(results))
	}
}

func TestListAllSessions_SkipsSubagents(t *testing.T) {
	r, baseDir := setupTestReader(t)
	projDir := filepath.Join(baseDir, "project1")
	subDir := filepath.Join(projDir, "subagents")

	jsonl := `{"type":"user","sessionId":"main","cwd":"/repo","timestamp":"2026-02-26T10:00:00Z","message":{"role":"user","content":"main"}}
`
	subJsonl := `{"type":"user","sessionId":"sub","cwd":"/repo","timestamp":"2026-02-26T10:00:00Z","message":{"role":"user","content":"sub"}}
`
	writeJSONL(t, projDir, "main.jsonl", jsonl)
	writeJSONL(t, subDir, "sub.jsonl", subJsonl)

	results := r.ListAllSessions(0, 0, 0)
	if len(results) != 1 {
		t.Errorf("expected 1 session (subagent excluded), got %d", len(results))
	}
	if results[0].SessionID != "main" {
		t.Errorf("expected 'main' session, got %q", results[0].SessionID)
	}
}

func TestExtractTextContent(t *testing.T) {
	// String content
	got := extractTextContent("hello world")
	if got != "hello world" {
		t.Errorf("extractTextContent(string) = %q", got)
	}

	// Array content with text block
	blocks := []any{
		map[string]any{"type": "text", "text": "extracted text"},
	}
	got = extractTextContent(blocks)
	if got != "extracted text" {
		t.Errorf("extractTextContent(array) = %q", got)
	}

	// Nil content
	got = extractTextContent(nil)
	if got != "" {
		t.Errorf("extractTextContent(nil) = %q", got)
	}
}
