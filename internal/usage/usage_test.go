package usage

import (
	"os"
	"path/filepath"
	"testing"
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

func TestReadSessionByID(t *testing.T) {
	r, baseDir := setupTestReader(t)
	projDir := filepath.Join(baseDir, "project1")

	jsonl := `{"type":"user","sessionId":"sess-001","cwd":"/repo","timestamp":"2026-02-26T10:00:00Z","message":{"role":"user","content":"hello"}}
{"type":"assistant","sessionId":"sess-001","timestamp":"2026-02-26T10:00:01Z","message":{"role":"assistant","model":"claude-opus-4-6","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}}
{"type":"assistant","sessionId":"sess-001","timestamp":"2026-02-26T10:00:02Z","message":{"role":"assistant","model":"claude-opus-4-6","usage":{"input_tokens":200,"output_tokens":100,"cache_creation_input_tokens":20,"cache_read_input_tokens":10}}}
`
	writeJSONL(t, projDir, "sess-001.jsonl", jsonl)

	stats := r.ReadSessionByID("sess-001")
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	if stats.SessionID != "sess-001" {
		t.Errorf("SessionID = %q", stats.SessionID)
	}
	if stats.InputTokens != 300 {
		t.Errorf("InputTokens = %d, want 300", stats.InputTokens)
	}
	if stats.OutputTokens != 150 {
		t.Errorf("OutputTokens = %d, want 150", stats.OutputTokens)
	}
	if stats.CacheCreationInputTokens != 30 {
		t.Errorf("CacheCreationInputTokens = %d, want 30", stats.CacheCreationInputTokens)
	}
	if stats.CacheReadInputTokens != 15 {
		t.Errorf("CacheReadInputTokens = %d, want 15", stats.CacheReadInputTokens)
	}
	if stats.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q", stats.Model)
	}
	if stats.EstimatedCostUSD <= 0 {
		t.Error("expected positive estimated cost")
	}
}

func TestReadSessionByID_NotFound(t *testing.T) {
	r, _ := setupTestReader(t)
	stats := r.ReadSessionByID("nonexistent")
	if stats != nil {
		t.Error("expected nil for non-existent session")
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
	if info.Tokens.InputTokens != 500 {
		t.Errorf("Tokens.InputTokens = %d, want 500", info.Tokens.InputTokens)
	}
}

func TestReadSessionByWorkDir(t *testing.T) {
	r, baseDir := setupTestReader(t)
	projDir := filepath.Join(baseDir, "project1")

	jsonl := `{"type":"user","sessionId":"sess-003","cwd":"/home/user/myrepo","timestamp":"2026-02-26T10:00:00Z","message":{"role":"user","content":"test"}}
{"type":"assistant","sessionId":"sess-003","timestamp":"2026-02-26T10:00:01Z","message":{"role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
`
	writeJSONL(t, projDir, "sess-003.jsonl", jsonl)

	stats := r.ReadSessionByWorkDir("/home/user/myrepo")
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	if stats.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", stats.InputTokens)
	}
}

func TestReadSessionByWorkDir_SubDir(t *testing.T) {
	r, baseDir := setupTestReader(t)
	projDir := filepath.Join(baseDir, "project1")

	jsonl := `{"type":"user","sessionId":"sess-004","cwd":"/home/user/myrepo","timestamp":"2026-02-26T10:00:00Z","message":{"role":"user","content":"test"}}
{"type":"assistant","sessionId":"sess-004","timestamp":"2026-02-26T10:00:01Z","message":{"role":"assistant","usage":{"input_tokens":50,"output_tokens":25,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
`
	writeJSONL(t, projDir, "sess-004.jsonl", jsonl)

	// Should match when searching for a subdirectory of the CWD
	stats := r.ReadSessionByWorkDir("/home/user/myrepo/workspace-1")
	if stats == nil {
		t.Fatal("expected match for subdirectory")
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

func TestPathMatches(t *testing.T) {
	tests := []struct {
		cwd     string
		workDir string
		want    bool
	}{
		{"/home/user/repo", "/home/user/repo", true},
		{"/home/user/repo", "/home/user/repo/sub", true},
		{"/home/user/repo", "/home/user/repo-other", false},
		{"/home/user/repo", "/other/path", false},
	}
	for _, tt := range tests {
		got := pathMatches(tt.cwd, tt.workDir)
		if got != tt.want {
			t.Errorf("pathMatches(%q, %q) = %v, want %v", tt.cwd, tt.workDir, got, tt.want)
		}
	}
}

func TestEstimateCost(t *testing.T) {
	stats := TokenStats{
		InputTokens:              1_000_000,
		OutputTokens:             1_000_000,
		CacheCreationInputTokens: 1_000_000,
		CacheReadInputTokens:     1_000_000,
	}
	cost := estimateCost(stats)
	// 15 + 75 + 18.75 + 1.5 = 110.25
	if cost < 110 || cost > 111 {
		t.Errorf("estimateCost = %.2f, expected ~110.25", cost)
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
