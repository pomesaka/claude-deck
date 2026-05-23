package usage

import (
	"bufio"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"io"
	"os"
	"strings"
	"time"
)

const codexRuntimeActivityTailBytes int64 = 512 * 1024

type RuntimeActivityKind int

const (
	// RuntimeActivityNone means the transcript tail contains no status signal.
	RuntimeActivityNone RuntimeActivityKind = iota
	// RuntimeActivityRunning means the runtime is actively processing a turn.
	RuntimeActivityRunning
	// RuntimeActivityIdle means the runtime completed the current turn.
	RuntimeActivityIdle
)

// RuntimeActivity is the small realtime projection extracted from transcript writes.
type RuntimeActivity struct {
	SessionID   string
	Kind        RuntimeActivityKind
	CurrentTool string
	ClearTool   bool
}

type codexEntry struct {
	Timestamp string         `json:"timestamp"`
	Type      string         `json:"type"`
	Payload   jsontext.Value `json:"payload"`
}

type codexSessionMeta struct {
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	CWD           string `json:"cwd"`
	Originator    string `json:"originator"`
	CLIVersion    string `json:"cli_version"`
	ModelProvider string `json:"model_provider"`
}

type codexTurnContext struct {
	CWD            string `json:"cwd"`
	Model          string `json:"model"`
	ApprovalPolicy string `json:"approval_policy"`
}

type codexEventMsg struct {
	Type        string          `json:"type"`
	Message     string          `json:"message"`
	LastMessage string          `json:"last_agent_message"`
	StartedAt   int64           `json:"started_at"`
	CompletedAt int64           `json:"completed_at"`
	Info        *codexTokenInfo `json:"info"`
	RateLimits  jsontext.Value  `json:"rate_limits,omitempty"`
}

type codexTokenInfo struct {
	TotalTokenUsage codexTokenUsage `json:"total_token_usage"`
	LastTokenUsage  codexTokenUsage `json:"last_token_usage"`
	ContextWindow   int             `json:"model_context_window"`
}

type codexTokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type codexResponseItem struct {
	Type      string         `json:"type"`
	Role      string         `json:"role"`
	Name      string         `json:"name"`
	CallID    string         `json:"call_id"`
	Arguments string         `json:"arguments"`
	Content   jsontext.Value `json:"content"`
	Summary   jsontext.Value `json:"summary"`
}

func (r *Reader) extractCodexInfoQuick(path string, mtime time.Time) *SessionInfo {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	info := SessionInfo{
		SessionID:    r.sessionIDFromPath(path),
		LastActivity: mtime,
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		var entry codexEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339Nano, entry.Timestamp); err == nil {
				if info.StartedAt.IsZero() || t.Before(info.StartedAt) {
					info.StartedAt = t
				}
			}
		}
		r.accumulateCodexEntry(&info, &entry)
		if info.CWD != "" && info.Prompt != "" && !info.StartedAt.IsZero() {
			break
		}
	}

	if info.CWD == "" {
		return nil
	}
	return &info
}

func (r *Reader) extractCodexInfo(path string) *SessionInfo {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	info := SessionInfo{SessionID: r.sessionIDFromPath(path)}
	dec := jsontext.NewDecoder(f)
	for {
		var entry codexEntry
		if err := json.UnmarshalDecode(dec, &entry); err != nil {
			if err == io.EOF {
				break
			}
			continue
		}
		if entry.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339Nano, entry.Timestamp); err == nil {
				if info.StartedAt.IsZero() || t.Before(info.StartedAt) {
					info.StartedAt = t
				}
				if t.After(info.LastActivity) {
					info.LastActivity = t
				}
			}
		}
		r.accumulateCodexEntry(&info, &entry)
	}
	if info.CWD == "" {
		return nil
	}
	info.Tokens.SessionID = info.SessionID
	info.Tokens.EstimatedCostUSD = estimateCost(info.Tokens)
	return &info
}

func (r *Reader) accumulateCodexEntry(info *SessionInfo, entry *codexEntry) {
	switch entry.Type {
	case "session_meta":
		var meta codexSessionMeta
		if json.Unmarshal(entry.Payload, &meta) == nil {
			if meta.ID != "" {
				info.SessionID = meta.ID
			}
			if meta.CWD != "" {
				info.CWD = meta.CWD
			}
			if meta.Timestamp != "" && info.StartedAt.IsZero() {
				if t, err := time.Parse(time.RFC3339Nano, meta.Timestamp); err == nil {
					info.StartedAt = t
				}
			}
		}
	case "turn_context":
		var tc codexTurnContext
		if json.Unmarshal(entry.Payload, &tc) == nil {
			if tc.CWD != "" {
				info.CWD = tc.CWD
			}
			if tc.Model != "" {
				info.Model = tc.Model
				info.Tokens.Model = tc.Model
			}
			if tc.ApprovalPolicy != "" {
				info.PermissionMode = tc.ApprovalPolicy
			}
		}
	case "event_msg":
		var ev codexEventMsg
		if json.Unmarshal(entry.Payload, &ev) != nil {
			return
		}
		if ev.Type == "user_message" && info.Prompt == "" {
			info.Prompt = ev.Message
		}
		if ev.Info != nil {
			applyCodexTokenUsage(&info.Tokens, ev.Info.TotalTokenUsage)
		}
		if ev.StartedAt > 0 && info.StartedAt.IsZero() {
			info.StartedAt = time.Unix(ev.StartedAt, 0)
		}
		if ev.CompletedAt > 0 {
			t := time.Unix(ev.CompletedAt, 0)
			if t.After(info.LastActivity) {
				info.LastActivity = t
			}
		}
	}
}

func (r *Reader) readCodexTokensByID(sessionID string) *TokenStats {
	path := r.ResolveSessionPath(sessionID)
	if path == "" {
		return nil
	}
	return r.aggregateCodexFile(path)
}

func (r *Reader) aggregateCodexFile(path string) *TokenStats {
	info := r.extractCodexInfo(path)
	if info == nil {
		return nil
	}
	stats := info.Tokens
	stats.SessionID = info.SessionID
	stats.EstimatedCostUSD = estimateCost(stats)
	return &stats
}

func applyCodexTokenUsage(stats *TokenStats, usage codexTokenUsage) {
	stats.InputTokens = usage.InputTokens
	stats.OutputTokens = usage.OutputTokens
	stats.CacheCreationInputTokens = usage.CacheCreationInputTokens
	stats.CacheReadInputTokens = usage.CacheReadInputTokens
}

func processCodexEntry(line []byte, entries *[]LogEntry) bool {
	var entry codexEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return false
	}
	switch entry.Type {
	case "event_msg":
		var ev codexEventMsg
		if json.Unmarshal(entry.Payload, &ev) != nil {
			return false
		}
		switch ev.Type {
		case "user_message":
			text := firstLine(ev.Message)
			if text != "" {
				*entries = append(*entries, LogEntry{Kind: LogEntryUser, Text: text})
				return true
			}
		case "agent_message":
			if strings.TrimSpace(ev.Message) != "" {
				*entries = append(*entries, LogEntry{Kind: LogEntryText, Text: ev.Message})
				return true
			}
		case "task_started":
			*entries = append(*entries, LogEntry{Kind: LogEntryThinking, Text: "Running"})
			return true
		}
	case "response_item":
		var item codexResponseItem
		if json.Unmarshal(entry.Payload, &item) != nil {
			return false
		}
		return processCodexResponseItem(item, entries)
	}
	return false
}

func processCodexResponseItem(item codexResponseItem, entries *[]LogEntry) bool {
	switch item.Type {
	case "message":
		text := codexMessageText(item.Content)
		if strings.TrimSpace(text) == "" {
			return false
		}
		kind := LogEntryText
		if item.Role == "user" {
			kind = LogEntryUser
			text = firstLine(text)
		}
		*entries = append(*entries, LogEntry{Kind: kind, Text: text})
		return true
	case "function_call":
		*entries = append(*entries, LogEntry{
			Kind:       LogEntryToolUse,
			ToolName:   item.Name,
			ToolDetail: firstLine(item.Arguments),
			ToolID:     item.CallID,
		})
		return true
	case "function_call_output":
		if len(*entries) > 0 {
			(*entries)[len(*entries)-1].HasResult = true
		}
		return true
	case "reasoning":
		*entries = append(*entries, LogEntry{Kind: LogEntryThinking, Text: "Reasoning"})
		return true
	}
	return false
}

func codexMessageText(raw jsontext.Value) string {
	if len(raw) == 0 {
		return ""
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) == nil {
		var sb strings.Builder
		for _, block := range blocks {
			if text, ok := block["text"].(string); ok {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(text)
			}
		}
		return sb.String()
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func firstLine(text string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return first
}

func (r *Reader) ReadRuntimeActivity(path string) RuntimeActivity {
	if r.layout != TranscriptCodex {
		return RuntimeActivity{}
	}
	return r.readCodexRuntimeActivity(path)
}

func (r *Reader) readCodexRuntimeActivity(path string) RuntimeActivity {
	f, err := os.Open(path)
	if err != nil {
		return RuntimeActivity{}
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return RuntimeActivity{}
	}
	offset := fi.Size() - codexRuntimeActivityTailBytes
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return RuntimeActivity{}
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	if offset > 0 && scanner.Scan() {
		// Drop a possibly partial first line.
	}

	activity := RuntimeActivity{SessionID: r.sessionIDFromPath(path)}
	for scanner.Scan() {
		next, ok := codexRuntimeActivityFromLine(scanner.Bytes())
		if ok {
			next.SessionID = activity.SessionID
			activity = next
		}
	}
	return activity
}

func codexRuntimeActivityFromLine(line []byte) (RuntimeActivity, bool) {
	var entry codexEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return RuntimeActivity{}, false
	}
	switch entry.Type {
	case "event_msg":
		var ev codexEventMsg
		if json.Unmarshal(entry.Payload, &ev) != nil {
			return RuntimeActivity{}, false
		}
		switch ev.Type {
		case "task_started":
			return RuntimeActivity{Kind: RuntimeActivityRunning, ClearTool: true}, true
		case "task_complete":
			return RuntimeActivity{Kind: RuntimeActivityIdle, ClearTool: true}, true
		}
	case "response_item":
		var item codexResponseItem
		if json.Unmarshal(entry.Payload, &item) != nil {
			return RuntimeActivity{}, false
		}
		switch item.Type {
		case "function_call":
			return RuntimeActivity{Kind: RuntimeActivityRunning, CurrentTool: item.Name}, true
		case "function_call_output":
			return RuntimeActivity{Kind: RuntimeActivityRunning, ClearTool: true}, true
		}
	}
	return RuntimeActivity{}, false
}
