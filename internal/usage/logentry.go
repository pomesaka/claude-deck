package usage

import (
	"fmt"
	"hash/fnv"
	"path/filepath"
	"strconv"
	"strings"
)

// LogEntryKind categorizes a log entry for rendering.
type LogEntryKind int

const (
	LogEntryUser     LogEntryKind = iota // user prompt
	LogEntryText                         // assistant text (markdown)
	LogEntryToolUse                      // tool invocation
	LogEntryThinking                     // thinking block (collapsed indicator)
	LogEntryDiff                         // file diff (Edit/Write structured patch)
)

// LogEntry is a structured representation of a single logical block
// in a Claude Code session log.
type LogEntry struct {
	Kind       LogEntryKind
	Text       string // markdown text or user prompt first line
	ToolName   string // LogEntryToolUse only
	ToolDetail string // primary argument (command, file_path, etc.)
	ToolID     string // tool_use id (for result matching)
	HasResult  bool   // true if a corresponding tool_result was found
	Depth      int    // 0=parent, 1+=subagent nesting level
}

// ReadSessionLogEntriesByID reads a Claude Code session's JSONL file and returns
// structured log entries for rich rendering (one-shot convenience wrapper).
func (r *Reader) ReadSessionLogEntriesByID(sessionID string) []LogEntry {
	path := r.ResolveSessionPath(sessionID)
	if path == "" {
		return nil
	}
	s := NewLogStreamer(path)
	s.ReadAll()
	return s.Entries()
}

// ResolveSessionPath returns the JSONL file path for a session ID, or "" if not found.
func (r *Reader) ResolveSessionPath(sessionID string) string {
	pattern := filepath.Join(r.baseDir, "*", sessionID+".jsonl")
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// markToolResults scans a tool_result user message and marks corresponding
// tool_use entries as having results.
func markToolResults(content any, entries []LogEntry, toolIndex map[string]int) {
	arr, ok := content.([]any)
	if !ok {
		return
	}
	for _, block := range arr {
		m, ok := block.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] != "tool_result" {
			continue
		}
		toolUseID, _ := m["tool_use_id"].(string)
		if toolUseID == "" {
			continue
		}
		if idx, ok := toolIndex[toolUseID]; ok && idx < len(entries) {
			entries[idx].HasResult = true
		}
	}
}

// extractAssistantLogEntries extracts structured log entries from an assistant message.
// Consecutive text blocks are merged into a single LogEntryText.
func extractAssistantLogEntries(content any) []LogEntry {
	var entries []LogEntry
	switch v := content.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			entries = append(entries, LogEntry{Kind: LogEntryText, Text: v})
		}
	case []any:
		var textBuf strings.Builder
		flushText := func() {
			if textBuf.Len() > 0 {
				entries = append(entries, LogEntry{Kind: LogEntryText, Text: textBuf.String()})
				textBuf.Reset()
			}
		}
		for _, block := range v {
			m, ok := block.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "text":
				if text, ok := m["text"].(string); ok && strings.TrimSpace(text) != "" {
					if textBuf.Len() > 0 {
						textBuf.WriteString("\n")
					}
					textBuf.WriteString(text)
				}
			case "thinking":
				flushText()
				entries = append(entries, LogEntry{
					Kind: LogEntryThinking,
					Text: pickThinkingVerb(len(entries)),
				})
			case "tool_use":
				flushText()
				if name, ok := m["name"].(string); ok {
					input, _ := m["input"].(map[string]any)
					id, _ := m["id"].(string)
					entries = append(entries, makeToolEntry(name, input, id))
				}
			}
		}
		flushText()
	}
	return entries
}

// makeToolEntry creates a LogEntryToolUse with the most relevant input field extracted.
func makeToolEntry(name string, input map[string]any, id string) LogEntry {
	entry := LogEntry{
		Kind:     LogEntryToolUse,
		ToolName: name,
		ToolID:   id,
	}
	if input == nil {
		return entry
	}

	var detail string
	switch name {
	case "Bash":
		detail, _ = input["command"].(string)
	case "Read", "Write", "Edit":
		detail, _ = input["file_path"].(string)
	case "Glob":
		detail, _ = input["pattern"].(string)
	case "Grep":
		detail, _ = input["pattern"].(string)
	case "Task":
		detail, _ = input["description"].(string)
	case "WebFetch":
		detail, _ = input["url"].(string)
	case "WebSearch":
		detail, _ = input["query"].(string)
	case "LSP":
		if op, ok := input["operation"].(string); ok {
			detail = op
			if fp, ok := input["filePath"].(string); ok {
				detail += " " + fp
			}
		}
	default:
		for _, key := range []string{"file_path", "command", "pattern", "query", "description"} {
			if v, ok := input[key].(string); ok {
				detail = v
				break
			}
		}
	}

	// 最初の1行だけ使う
	if first, _, ok := strings.Cut(detail, "\n"); ok {
		detail = first
	}
	entry.ToolDetail = detail
	return entry
}

// makeDiffEntry creates a LogEntryDiff from a toolUseResult with structuredPatch.
// Text には unified diff 形式のテキストを格納し、ToolDetail にファイルパスを格納する。
func makeDiffEntry(result *jsonlToolUseResult) LogEntry {
	var sb strings.Builder
	for _, hunk := range result.StructuredPatch {
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n",
			hunk.OldStart, hunk.OldLines, hunk.NewStart, hunk.NewLines)
		for _, line := range hunk.Lines {
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}
	return LogEntry{
		Kind:       LogEntryDiff,
		Text:       strings.TrimRight(sb.String(), "\n"),
		ToolDetail: result.FilePath,
	}
}

// pickThinkingVerb returns a deterministic cute verb for a thinking block
// based on its position in the log, matching Claude Code's style.
func pickThinkingVerb(index int) string {
	h := fnv.New32a()
	h.Write([]byte(strconv.Itoa(index)))
	return thinkingVerbs[int(h.Sum32())%len(thinkingVerbs)]
}

// thinkingVerbs is a subset of Claude Code's spinner verbs.
var thinkingVerbs = []string{
	"Baking", "Beboppin'", "Blanching", "Bloviating",
	"Bootstrapping", "Brewing", "Calculating", "Canoodling",
	"Caramelizing", "Cascading", "Cerebrating", "Choreographing",
	"Churning", "Clauding", "Coalescing", "Cogitating",
	"Combobulating", "Composing", "Computing", "Concocting",
	"Contemplating", "Cooking", "Crafting", "Crunching",
	"Crystallizing", "Cultivating", "Deliberating", "Doodling",
	"Fermenting", "Finagling", "Flummoxing", "Forging",
	"Frolicking", "Gallivanting", "Garnishing", "Generating",
	"Germinating", "Grooving", "Harmonizing", "Hatching",
	"Hullaballooing", "Ideating", "Imagining", "Improvising",
	"Incubating", "Inferring", "Infusing", "Kneading",
	"Lollygagging", "Manifesting", "Marinating", "Meandering",
	"Metamorphosing", "Moonwalking", "Mulling", "Musing",
	"Nebulizing", "Noodling", "Orchestrating", "Percolating",
	"Philosophising", "Pollinating", "Pondering", "Pontificating",
	"Processing", "Puzzling", "Ruminating", "Simmering",
	"Spelunking", "Spinning", "Sprouting", "Stewing",
	"Synthesizing", "Tinkering", "Transmuting", "Undulating",
	"Unfurling", "Vibing", "Wandering", "Whisking",
	"Zigzagging",
}
