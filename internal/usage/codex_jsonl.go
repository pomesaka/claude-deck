package usage

import (
	"bufio"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"os"
	"path/filepath"
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
	RateLimits  *RuntimeRateLimits
}

// RuntimeRateLimits is the runtime-agnostic projection of subscription limits.
type RuntimeRateLimits struct {
	FiveHour          RuntimeRateLimitWindow
	FiveHourAvailable bool
	SevenDay          RuntimeRateLimitWindow
	SevenDayAvailable bool
}

// RuntimeRateLimitWindow holds a single runtime limit window.
type RuntimeRateLimitWindow struct {
	UsedPct  float64
	ResetsAt int64
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
	ApprovalPolicy string `json:"approval_policy"`
}

type codexEventMsg struct {
	Type        string         `json:"type"`
	Message     string         `json:"message"`
	LastMessage string         `json:"last_agent_message"`
	StartedAt   int64          `json:"started_at"`
	CompletedAt int64          `json:"completed_at"`
	RateLimits  jsontext.Value `json:"rate_limits,omitempty"`
}

type codexRateLimits struct {
	Primary   *codexRateLimitWindow `json:"primary"`
	Secondary *codexRateLimitWindow `json:"secondary"`
}

type codexRateLimitWindow struct {
	UsedPct       float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
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

// codexFormat reads Codex CLI transcripts:
// <baseDir>/YYYY/MM/DD/rollout-<timestamp>-<session UUID>.jsonl.
type codexFormat struct{}

func (codexFormat) files(baseDir string) []string {
	files, _ := filepath.Glob(filepath.Join(baseDir, "*", "*", "*", "*.jsonl"))
	return files
}

// sessionID returns the UUID at the end of the file name (its last five
// dash-separated parts), or the whole name when it is not a rollout file.
func (codexFormat) sessionID(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if rest, ok := strings.CutPrefix(name, "rollout-"); ok {
		parts := strings.Split(rest, "-")
		if len(parts) >= 5 {
			return strings.Join(parts[len(parts)-5:], "-")
		}
	}
	return name
}

func (codexFormat) userMessageMarker() []byte { return []byte(`"type":"user_message"`) }

func (codexFormat) logLine(s *LogStreamer, line []byte) bool {
	return processCodexEntry(line, &s.entries)
}

func (c codexFormat) quickInfo(path string, mtime time.Time) *SessionInfo {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	info := SessionInfo{
		SessionID:    c.sessionID(path),
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
		accumulateCodexEntry(&info, &entry)
		if info.CWD != "" && info.Prompt != "" && !info.StartedAt.IsZero() {
			break
		}
	}

	if info.CWD == "" {
		return nil
	}
	return &info
}

func (c codexFormat) info(path string) *SessionInfo {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	info := SessionInfo{SessionID: c.sessionID(path)}
	scanLines(f, func(line []byte) bool {
		var entry codexEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return true // skip malformed lines
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
		accumulateCodexEntry(&info, &entry)
		return true
	})
	if info.CWD == "" {
		return nil
	}
	return &info
}

func accumulateCodexEntry(info *SessionInfo, entry *codexEntry) {
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

func (c codexFormat) runtimeActivity(path string) RuntimeActivity {
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

	activity := RuntimeActivity{SessionID: c.sessionID(path)}
	for scanner.Scan() {
		next, ok := codexRuntimeActivityFromLine(scanner.Bytes())
		if ok {
			activity.merge(next)
		}
	}
	return activity
}

func (a *RuntimeActivity) merge(next RuntimeActivity) {
	sessionID := a.SessionID
	if next.RateLimits != nil {
		a.RateLimits = next.RateLimits
	}
	if next.Kind == RuntimeActivityNone && next.CurrentTool == "" && !next.ClearTool {
		a.SessionID = sessionID
		return
	}
	next.SessionID = sessionID
	if next.RateLimits == nil {
		next.RateLimits = a.RateLimits
	}
	*a = next
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
		limits := codexRateLimitsFromRaw(ev.RateLimits)
		switch ev.Type {
		case "task_started":
			return RuntimeActivity{Kind: RuntimeActivityRunning, ClearTool: true, RateLimits: limits}, true
		case "task_complete":
			return RuntimeActivity{Kind: RuntimeActivityIdle, ClearTool: true, RateLimits: limits}, true
		case "token_count":
			if limits != nil {
				return RuntimeActivity{RateLimits: limits}, true
			}
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

func codexRateLimitsFromRaw(raw jsontext.Value) *RuntimeRateLimits {
	if len(raw) == 0 {
		return nil
	}
	var limits codexRateLimits
	if json.Unmarshal(raw, &limits) != nil {
		return nil
	}
	var out RuntimeRateLimits
	applyCodexRateLimitWindow(&out, limits.Primary)
	applyCodexRateLimitWindow(&out, limits.Secondary)
	if !out.FiveHourAvailable && !out.SevenDayAvailable {
		return nil
	}
	return &out
}

func applyCodexRateLimitWindow(out *RuntimeRateLimits, w *codexRateLimitWindow) {
	if w == nil {
		return
	}
	switch w.WindowMinutes {
	case 300:
		out.FiveHour = RuntimeRateLimitWindow{UsedPct: w.UsedPct, ResetsAt: w.ResetsAt}
		out.FiveHourAvailable = true
	case 10080:
		out.SevenDay = RuntimeRateLimitWindow{UsedPct: w.UsedPct, ResetsAt: w.ResetsAt}
		out.SevenDayAvailable = true
	}
}
