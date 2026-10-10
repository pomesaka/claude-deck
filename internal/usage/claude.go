package usage

import (
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
}
