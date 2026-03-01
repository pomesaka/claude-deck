package usage

import (
	"bufio"
	"context"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
)

// LogStreamer reads a JSONL file as structured log entries.
// Use Run for blocking event-driven streaming, or ReadAll for one-shot reads.
type LogStreamer struct {
	path      string
	entries   []LogEntry
	toolIndex map[string]int
	depth     int // サブエージェントの再帰深度（0=親）

	// parentToolUseID → agentID (progress エントリから収集)
	agentMap      map[string]string
	inlinedAgents map[string]bool // 既にインライン展開済みのエージェント
}

// NewLogStreamer creates a streamer for the given JSONL file path.
func NewLogStreamer(path string) *LogStreamer {
	return &LogStreamer{
		path:          path,
		toolIndex:     make(map[string]int),
		agentMap:      make(map[string]string),
		inlinedAgents: make(map[string]bool),
	}
}

// MaxEntries is the maximum number of log entries to keep. Override from config before use.
var MaxEntries = 500

// Entries returns the accumulated log entries (capped at MaxEntries).
func (s *LogStreamer) Entries() []LogEntry {
	if len(s.entries) > MaxEntries {
		return s.entries[len(s.entries)-MaxEntries:]
	}
	return s.entries
}

// ReadAll reads all existing entries from the JSONL file (one-shot, non-blocking).
// 行単位でデコードするため、壊れた行があってもスキップして続行する。
func (s *LogStreamer) ReadAll() {
	f, err := os.Open(s.path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		var entry jsonlEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue // skip malformed lines
		}
		s.processEntry(&entry)
	}
}

// ReadTail reads the last tailBytes of the JSONL file for quick initial display.
// Returns the file size at time of read (for use as RunFrom offset).
func (s *LogStreamer) ReadTail(tailBytes int64) int64 {
	f, err := os.Open(s.path)
	if err != nil {
		return 0
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	fileSize := fi.Size()

	offset := max(0, fileSize-tailBytes)
	if offset > 0 {
		if _, err := f.Seek(offset, 0); err != nil {
			return 0
		}
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	// offset > 0 なら最初の不完全行を読み捨て
	if offset > 0 {
		scanner.Scan()
	}

	for scanner.Scan() {
		var entry jsonlEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		s.processEntry(&entry)
	}
	return fileSize
}

// Run starts event-driven streaming of the JSONL file from the beginning.
// ctx がキャンセルされるまでブロックする。onChange はエントリ更新時に呼ばれる。
func (s *LogStreamer) Run(ctx context.Context, onChange func([]LogEntry)) error {
	return s.RunFrom(ctx, 0, onChange)
}

// RunFrom starts streaming from the given file offset.
// ReadTail で取得した fileSize を渡すことで、既読部分をスキップする。
func (s *LogStreamer) RunFrom(ctx context.Context, offset int64, onChange func([]LogEntry)) error {
	tr, err := NewTailReader(ctx, s.path, offset)
	if err != nil {
		return err
	}
	defer tr.Close()

	scanner := bufio.NewScanner(tr)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		var entry jsonlEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue // skip malformed lines
		}
		if s.processEntry(&entry) && onChange != nil {
			onChange(s.Entries())
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return scanner.Err()
}

// processEntry handles a single JSONL entry, returns true if entries changed.
func (s *LogStreamer) processEntry(entry *jsonlEntry) bool {
	// progress エントリからサブエージェント情報を収集し、即座にインライン展開
	if entry.Type == "progress" && entry.Data != nil && entry.Data.Type == "agent_progress" {
		agentID := entry.Data.AgentID
		if entry.ParentToolUseID != "" && agentID != "" {
			s.agentMap[entry.ParentToolUseID] = agentID
			if s.depth == 0 && !s.inlinedAgents[agentID] {
				s.inlinedAgents[agentID] = true
				return s.inlineAgent(agentID)
			}
		}
		return false
	}

	if entry.Message == nil {
		return false
	}

	content := entry.Message.parseContent()

	switch entry.Type {
	case "user":
		if isToolResult(content) {
			markToolResults(content, s.entries, s.toolIndex)
			// toolUseResult に structuredPatch があれば diff エントリを追加
			if entry.ToolUseResult != nil && len(entry.ToolUseResult.StructuredPatch) > 0 {
				s.entries = append(s.entries, makeDiffEntry(entry.ToolUseResult))
			}
			return true
		}
		text := extractTextContent(content)
		if text != "" {
			first, _, _ := strings.Cut(text, "\n")
			s.entries = append(s.entries, LogEntry{
				Kind: LogEntryUser,
				Text: first,
			})
			return true
		}
	case "assistant":
		newEntries := extractAssistantLogEntries(content)
		if len(newEntries) == 0 {
			return false
		}
		for _, e := range newEntries {
			if e.Kind == LogEntryToolUse && e.ToolID != "" {
				s.toolIndex[e.ToolID] = len(s.entries)
			}
			s.entries = append(s.entries, e)
		}
		return true
	}
	return false
}

// inlineAgent reads a subagent's JSONL file and appends its entries.
// Returns true if any entries were added.
func (s *LogStreamer) inlineAgent(agentID string) bool {
	sessionDir := strings.TrimSuffix(s.path, ".jsonl")
	agentPath := filepath.Join(sessionDir, "subagents", "agent-"+agentID+".jsonl")
	subEntries := readSubagentEntries(agentPath)
	if len(subEntries) == 0 {
		return false
	}
	s.entries = append(s.entries, subEntries...)
	return true
}

// readSubagentEntries reads a subagent JSONL file and returns entries with Depth=1.
func readSubagentEntries(path string) []LogEntry {
	sub := NewLogStreamer(path)
	sub.depth = 1 // 再帰防止
	sub.ReadAll()
	entries := sub.Entries()
	for i := range entries {
		entries[i].Depth = 1
	}
	return entries
}
