package usage

import (
	"bufio"
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// claudeFormat reads Claude Code transcripts: <baseDir>/<project>/<session UUID>.jsonl.
type claudeFormat struct{}

func (claudeFormat) files(baseDir string) []string {
	files, _ := filepath.Glob(filepath.Join(baseDir, "*", "*.jsonl"))
	return files
}

// sessionID returns the file name without its extension:
// "/path/to/259bcba0-aa94.jsonl" → "259bcba0-aa94".
func (claudeFormat) sessionID(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

func (claudeFormat) userMessageMarker() []byte { return []byte(`"type":"user"`) }

// runtimeActivity reports nothing.
// WHY: Claude Code は deck-status プラグインが状態を store に書くので、JSONL からは読まない。
func (claudeFormat) runtimeActivity(string) RuntimeActivity { return RuntimeActivity{} }

func (claudeFormat) logLine(s *LogStreamer, line []byte) bool {
	var entry jsonlEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return false // skip malformed lines
	}
	return s.processEntry(&entry)
}

// tokenOnlyEntry is a minimal struct for fast token aggregation.
// jsonv2 は宣言されたフィールドだけデコードし、巨大な content 等をスキップする。
type tokenOnlyEntry struct {
	Timestamp string            `json:"timestamp"`
	Message   *tokenOnlyMessage `json:"message,omitempty"`
}

type tokenOnlyMessage struct {
	Model string      `json:"model"`
	Usage *jsonlUsage `json:"usage,omitempty"`
}

// tokens reads only the token usage from a session's JSONL file.
// 行単位で "usage" を含むかバイト検索し、該当行だけデコードすることで
// 巨大な content を持つ行のパースを完全にスキップする。
func (claudeFormat) tokens(path, sessionID string) *TokenStats {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	stats := TokenStats{SessionID: sessionID}
	usageMarker := []byte(`"usage"`)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // max 10MB/line
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.Contains(line, usageMarker) {
			continue
		}
		var entry tokenOnlyEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.Message != nil && entry.Message.Usage != nil {
			u := entry.Message.Usage
			stats.InputTokens += u.InputTokens
			stats.OutputTokens += u.OutputTokens
			stats.CacheCreationInputTokens += u.CacheCreationInputTokens
			stats.CacheReadInputTokens += u.CacheReadInputTokens
			if entry.Message.Model != "" {
				stats.Model = entry.Message.Model
			}
		}
	}

	stats.EstimatedCostUSD = estimateCost(stats)
	return &stats
}

// quickInfo reads only the first few entries of a JSONL file
// to get basic session metadata (CWD, prompt, permissions).
// mtime is used as LastActivity approximation to avoid reading the entire file.
func (c claudeFormat) quickInfo(path string, mtime time.Time) *SessionInfo {
	fileSessionID := c.sessionID(path)

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	dec := jsontext.NewDecoder(f)
	var info SessionInfo
	info.SessionID = fileSessionID
	info.LastActivity = mtime

	// 先頭20エントリだけ読む（CWD・prompt・permissionMode・開始時刻の取得に十分）
	for range 20 {
		var entry jsonlEntry
		if err := json.UnmarshalDecode(dec, &entry); err != nil {
			break
		}
		if info.CWD == "" && entry.CWD != "" {
			info.CWD = entry.CWD
		}
		if entry.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339Nano, entry.Timestamp); err == nil {
				if info.StartedAt.IsZero() || t.Before(info.StartedAt) {
					info.StartedAt = t
				}
			}
		}
		if entry.Type == "user" {
			if entry.PermissionMode != "" {
				info.PermissionMode = entry.PermissionMode
			}
			if info.Prompt == "" && entry.Message != nil {
				info.Prompt = extractTextContent(entry.Message.parseContent())
			}
		}
	}

	if info.CWD == "" {
		return nil
	}
	return &info
}

// info reads all session metadata from a JSONL file.
// The session ID is derived from the filename (not from entry content),
// because --resume can mix entries from a previous session into the file.
func (c claudeFormat) info(path string) *SessionInfo {
	// ファイル名がセッション ID（例: 259bcba0-...aa94.jsonl → 259bcba0-...aa94）
	fileSessionID := c.sessionID(path)

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var info SessionInfo
	info.SessionID = fileSessionID

	scanLines(f, func(line []byte) bool {
		var entry jsonlEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return true // skip malformed lines
		}
		if info.CWD == "" && entry.CWD != "" {
			info.CWD = entry.CWD
		}
		accumulateEntry(&info, &entry)
		return true
	})

	if info.CWD == "" {
		return nil
	}

	info.Tokens.SessionID = info.SessionID
	info.Tokens.EstimatedCostUSD = estimateCost(info.Tokens)
	return &info
}

// accumulateEntry merges a single JSONL entry into SessionInfo.
func accumulateEntry(info *SessionInfo, entry *jsonlEntry) {
	// Track timestamps
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

	// Extract metadata from user entries
	if entry.Type == "user" {
		if entry.PermissionMode != "" {
			info.PermissionMode = entry.PermissionMode
		}
		if entry.GitBranch != "" {
			info.GitBranch = entry.GitBranch
		}
		// First user message becomes the prompt (lazy deserialize only once)
		if info.Prompt == "" && entry.Message != nil {
			info.Prompt = extractTextContent(entry.Message.parseContent())
		}
	}

	// Accumulate token usage from assistant entries
	if entry.Message != nil {
		accumulateUsage(&info.Tokens, entry.Message)
		if entry.Message.Model != "" {
			info.Model = entry.Message.Model
		}
	}
}

// accumulateUsage adds token counts and model from msg into stats.
// No-op if msg or msg.Usage is nil.
func accumulateUsage(stats *TokenStats, msg *jsonlMessage) {
	if msg == nil || msg.Usage == nil {
		return
	}
	u := msg.Usage
	stats.InputTokens += u.InputTokens
	stats.OutputTokens += u.OutputTokens
	stats.CacheCreationInputTokens += u.CacheCreationInputTokens
	stats.CacheReadInputTokens += u.CacheReadInputTokens
	if msg.Model != "" {
		stats.Model = msg.Model
	}
}
