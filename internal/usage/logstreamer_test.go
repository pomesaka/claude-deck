package usage

import (
	"path/filepath"
	"slices"
	"testing"
)

// A tool_result line marks its tool call as done whatever shape toolUseResult
// has, and adds a diff entry only for a structured patch.
func TestLogStreamer_ToolUseResultShapes(t *testing.T) {
	const toolUse = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"a.go"}}]}}`
	result := func(toolUseResult string) string {
		return `{"type":"user","toolUseResult":` + toolUseResult + `,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1"}]}}`
	}
	tests := []struct {
		name          string
		toolUseResult string
		wantKinds     []LogEntryKind
	}{
		{"string", `"Error: denied"`, []LogEntryKind{LogEntryToolUse}},
		{"array", `[{"type":"text","text":"ok"}]`, []LogEntryKind{LogEntryToolUse}},
		{"object without a patch", `{"filePath":"a.go","structuredPatch":[]}`, []LogEntryKind{LogEntryToolUse}},
		{"object with a patch", `{"filePath":"a.go","structuredPatch":[{"oldStart":1,"oldLines":1,"newStart":1,"newLines":1,"lines":["-a","+b"]}]}`, []LogEntryKind{LogEntryToolUse, LogEntryDiff}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeJSONL(t, dir, "s.jsonl", toolUse+"\n"+result(tt.toolUseResult)+"\n")

			s := NewLogStreamer(filepath.Join(dir, "s.jsonl"))
			s.ReadAll()

			var kinds []LogEntryKind
			for _, e := range s.Entries() {
				kinds = append(kinds, e.Kind)
			}
			if !slices.Equal(kinds, tt.wantKinds) {
				t.Fatalf("entry kinds = %v, want %v", kinds, tt.wantKinds)
			}
			if !s.Entries()[0].HasResult {
				t.Error("the tool call is not marked as done")
			}
		})
	}
}

func TestFormatFor(t *testing.T) {
	tests := []struct {
		provider string
		want     format
	}{
		{"claude", claudeFormat{}},
		{"codex", codexFormat{}},
		{"", claudeFormat{}},
	}
	for _, tt := range tests {
		if got := formatFor(tt.provider); got != tt.want {
			t.Errorf("formatFor(%q) = %T, want %T", tt.provider, got, tt.want)
		}
	}
}
