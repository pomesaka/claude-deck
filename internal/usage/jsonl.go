package usage

import (
	json "encoding/json/v2"
	"encoding/json/jsontext"
)

// jsonlEntry represents a single line in a Claude Code JSONL file.
type jsonlEntry struct {
	Type           string        `json:"type"`
	SessionID      string        `json:"sessionId"`
	CWD            string        `json:"cwd"`
	Timestamp      string        `json:"timestamp"`
	PermissionMode string        `json:"permissionMode,omitempty"`
	GitBranch      string        `json:"gitBranch,omitempty"`
	Message        *jsonlMessage `json:"message,omitempty"`

	// progress エントリ用 (type: "progress")
	Data            *jsonlProgressData   `json:"data,omitempty"`
	ParentToolUseID string               `json:"parentToolUseID,omitempty"`

	// tool_result エントリ用 (Edit/Write の差分)
	ToolUseResult *jsonlToolUseResult `json:"toolUseResult,omitempty"`
}

// jsonlProgressData holds the data field of a progress entry.
type jsonlProgressData struct {
	Type    string `json:"type"`
	AgentID string `json:"agentId"`
}

type jsonlMessage struct {
	Role    string         `json:"role"`
	Model   string         `json:"model"`
	Content jsontext.Value `json:"content,omitempty"`
	Usage   *jsonlUsage    `json:"usage,omitempty"`
}

// parseContent lazily deserializes the raw Content JSON into a Go value.
// Returns nil if Content is empty or malformed.
func (m *jsonlMessage) parseContent() any {
	if len(m.Content) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(m.Content, &v); err != nil {
		return nil
	}
	return v
}

type jsonlUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// jsonlToolUseResult holds the structured result of a tool execution (Edit/Write).
type jsonlToolUseResult struct {
	FilePath        string       `json:"filePath"`
	StructuredPatch []patchHunk  `json:"structuredPatch"`
}

// patchHunk represents a single hunk in a unified diff.
type patchHunk struct {
	OldStart int      `json:"oldStart"`
	OldLines int      `json:"oldLines"`
	NewStart int      `json:"newStart"`
	NewLines int      `json:"newLines"`
	Lines    []string `json:"lines"`
}

// isToolResult returns true if the message content is a tool_result response (not a user prompt).
func isToolResult(content any) bool {
	arr, ok := content.([]any)
	if !ok {
		return false
	}
	for _, block := range arr {
		if m, ok := block.(map[string]any); ok {
			if m["type"] == "tool_result" {
				return true
			}
		}
	}
	return false
}

// extractTextContent extracts text from a message content field.
// Content can be a string or an array of content blocks.
func extractTextContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		for _, block := range v {
			if m, ok := block.(map[string]any); ok {
				if m["type"] == "text" {
					if text, ok := m["text"].(string); ok {
						return text
					}
				}
			}
		}
	}
	return ""
}
