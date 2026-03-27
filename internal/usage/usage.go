// Package usage reads Claude Code's local JSONL session logs
// from ~/.claude/projects/ and aggregates token usage per session.
package usage

import (
	"bufio"
	"bytes"
	json "encoding/json/v2"
	"encoding/json/jsontext"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TokenStats holds aggregated token usage for a single Claude Code session.
type TokenStats struct {
	SessionID                string  `json:"session_id"`
	Model                    string  `json:"model"`
	InputTokens              int     `json:"input_tokens"`
	OutputTokens             int     `json:"output_tokens"`
	CacheCreationInputTokens int     `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int     `json:"cache_read_input_tokens"`
	EstimatedCostUSD         float64 `json:"estimated_cost_usd"`
}

// TotalTokens returns the total token count (input + output + cache).
func (t TokenStats) TotalTokens() int {
	return t.InputTokens + t.OutputTokens + t.CacheCreationInputTokens + t.CacheReadInputTokens
}

// SessionInfo holds session metadata extracted from a Claude Code JSONL file.
// This is the primary data source; claude-deck's store only holds supplementary metadata.
type SessionInfo struct {
	SessionID      string
	CWD            string
	Model          string
	PermissionMode string
	GitBranch      string
	Prompt         string // first user message content
	StartedAt      time.Time
	LastActivity   time.Time
	Tokens         TokenStats
}

// Reader reads Claude Code JSONL session data.
type Reader struct {
	baseDir string // ~/.claude/projects/
}

// NewReader creates a Reader. If baseDir is empty, defaults to ~/.claude/projects/.
func NewReader(baseDir string) *Reader {
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".claude", "projects")
	}
	return &Reader{baseDir: baseDir}
}

// BaseDir returns the base directory for Claude Code JSONL files.
func (r *Reader) BaseDir() string {
	return r.baseDir
}

// ReadSessionByWorkDir finds the Claude Code session whose cwd matches workDir
// and returns aggregated token stats. Returns nil if not found.
func (r *Reader) ReadSessionByWorkDir(workDir string) *TokenStats {
	jsonlFiles, _ := filepath.Glob(filepath.Join(r.baseDir, "*", "*.jsonl"))

	for _, path := range jsonlFiles {
		stats := r.scanFile(path, workDir)
		if stats != nil {
			return stats
		}
	}
	return nil
}

// DeleteSessionFiles removes the Claude Code JSONL file and session directory
// (subagents etc.) for the given session ID.
// Returns nil if nothing existed.
func (r *Reader) DeleteSessionFiles(sessionID string) error {
	// JSONL ファイル: <project>/<sessionID>.jsonl
	jsonlPattern := filepath.Join(r.baseDir, "*", sessionID+".jsonl")
	if matches, _ := filepath.Glob(jsonlPattern); len(matches) > 0 {
		for _, path := range matches {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	// セッションディレクトリ: <project>/<sessionID>/ (subagents 等)
	dirPattern := filepath.Join(r.baseDir, "*", sessionID)
	if matches, _ := filepath.Glob(dirPattern); len(matches) > 0 {
		for _, path := range matches {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReadSessionByID reads a specific Claude Code session by its UUID.
func (r *Reader) ReadSessionByID(sessionID string) *TokenStats {
	path := r.ResolveSessionPath(sessionID)
	if path == "" {
		return nil
	}
	return r.aggregateFile(path)
}

// ReadSessionInfoByID reads full session metadata for a specific Claude Code session.
func (r *Reader) ReadSessionInfoByID(sessionID string) *SessionInfo {
	path := r.ResolveSessionPath(sessionID)
	if path == "" {
		return nil
	}
	return r.extractInfo(path)
}

// HasConversation returns true if the session's JSONL contains at least one
// user message (type: "user"). /clear 後にメッセージを送らず終了したセッションは false。
func (r *Reader) HasConversation(sessionID string) bool {
	path := r.ResolveSessionPath(sessionID)
	if path == "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	// "type":"user" を含む行を高速バイト検索（デコード不要）
	marker := []byte(`"type":"user"`)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)
	for scanner.Scan() {
		if bytes.Contains(scanner.Bytes(), marker) {
			return true
		}
	}
	return false
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

// ReadTokensByID reads only token usage data from a session's JSONL file.
// 行単位で "usage" を含むかバイト検索し、該当行だけデコードすることで
// 巨大な content を持つ行のパースを完全にスキップする。
func (r *Reader) ReadTokensByID(sessionID string) *TokenStats {
	path := r.ResolveSessionPath(sessionID)
	if path == "" {
		return nil
	}

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

// ListAllSessions returns SessionInfo for JSONL files in the projects directory.
// Subagent files (under subagents/) are excluded.
// Sessions with no activity in the last maxAge are skipped (0 means no limit).
// limit controls the maximum number of sessions returned (0 means no limit);
// offset skips the first N eligible sessions (for pagination).
// files are sorted by mtime descending so the most recent sessions are processed first.
// 軽量スキャン: 各ファイルの先頭数エントリだけ読み、mtime を LastActivity として使う。
func (r *Reader) ListAllSessions(maxAge time.Duration, limit, offset int) []*SessionInfo {
	jsonlFiles, _ := filepath.Glob(filepath.Join(r.baseDir, "*", "*.jsonl"))

	// subagent ファイルを除外し、mtime をキャッシュしてソート（新しい順）
	type fileEntry struct {
		path  string
		mtime time.Time
	}
	var filtered []fileEntry
	for _, path := range jsonlFiles {
		if isSubagentPath(path) {
			continue
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		filtered = append(filtered, fileEntry{path: path, mtime: fi.ModTime()})
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].mtime.After(filtered[j].mtime)
	})

	var cutoff time.Time
	if maxAge > 0 {
		cutoff = time.Now().Add(-maxAge)
	}

	skipped := 0
	var results []*SessionInfo
	for _, fe := range filtered {
		if !cutoff.IsZero() && fe.mtime.Before(cutoff) {
			continue
		}
		info := r.extractInfoQuick(fe.path, fe.mtime)
		if info == nil {
			continue
		}
		if skipped < offset {
			skipped++
			continue
		}
		results = append(results, info)
		if limit > 0 && len(results) >= limit {
			break
		}
	}
	return results
}

// extractInfoQuick reads only the first few entries of a JSONL file
// to get basic session metadata (CWD, prompt, permissions).
// mtime is used as LastActivity approximation to avoid reading the entire file.
func (r *Reader) extractInfoQuick(path string, mtime time.Time) *SessionInfo {
	fileSessionID := sessionIDFromPath(path)

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

// extractInfo reads all session metadata from a JSONL file.
// The session ID is derived from the filename (not from entry content),
// because --resume can mix entries from a previous session into the file.
func (r *Reader) extractInfo(path string) *SessionInfo {
	// ファイル名がセッション ID（例: 259bcba0-...aa94.jsonl → 259bcba0-...aa94）
	fileSessionID := sessionIDFromPath(path)

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	dec := jsontext.NewDecoder(f)
	var info SessionInfo
	info.SessionID = fileSessionID

	for {
		var entry jsonlEntry
		if err := json.UnmarshalDecode(dec, &entry); err != nil {
			if err == io.EOF {
				break
			}
			continue
		}

		if info.CWD == "" && entry.CWD != "" {
			info.CWD = entry.CWD
		}

		r.accumulateEntry(&info, &entry)
	}

	if info.CWD == "" {
		return nil
	}

	info.Tokens.SessionID = info.SessionID
	info.Tokens.EstimatedCostUSD = estimateCost(info.Tokens)
	return &info
}

// accumulateEntry merges a single JSONL entry into SessionInfo.
func (r *Reader) accumulateEntry(info *SessionInfo, entry *jsonlEntry) {
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

// scanFile reads a JSONL file and checks if the cwd matches workDir.
// If it matches, aggregates all token usage from the file.
func (r *Reader) scanFile(path, workDir string) *TokenStats {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	dec := jsontext.NewDecoder(f)
	var stats TokenStats
	matched := false

	for {
		var entry jsonlEntry
		if err := json.UnmarshalDecode(dec, &entry); err != nil {
			if err == io.EOF {
				break
			}
			continue
		}

		if !matched && entry.CWD != "" {
			if pathMatches(entry.CWD, workDir) {
				matched = true
				stats.SessionID = entry.SessionID
			}
		}

		if !matched {
			continue
		}

		accumulateUsage(&stats, entry.Message)
	}

	if !matched {
		return nil
	}

	stats.EstimatedCostUSD = estimateCost(stats)
	return &stats
}

// aggregateFile reads all token usage from a JSONL file.
func (r *Reader) aggregateFile(path string) *TokenStats {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	dec := jsontext.NewDecoder(f)
	stats := TokenStats{
		SessionID: sessionIDFromPath(path),
	}

	for {
		var entry jsonlEntry
		if err := json.UnmarshalDecode(dec, &entry); err != nil {
			if err == io.EOF {
				break
			}
			continue
		}

		accumulateUsage(&stats, entry.Message)
	}

	if stats.SessionID == "" {
		return nil
	}

	stats.EstimatedCostUSD = estimateCost(stats)
	return &stats
}

// pathMatches checks if cwd matches or is a prefix of workDir.
func pathMatches(cwd, workDir string) bool {
	cwd = filepath.Clean(cwd)
	workDir = filepath.Clean(workDir)
	return cwd == workDir || strings.HasPrefix(workDir, cwd+string(filepath.Separator))
}

// sessionIDFromPath extracts the session UUID from a JSONL file path.
// e.g., "/path/to/259bcba0-aa94.jsonl" → "259bcba0-aa94"
func sessionIDFromPath(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

// isSubagentPath returns true if the path is inside a "subagents" directory.
func isSubagentPath(path string) bool {
	return strings.Contains(path, string(filepath.Separator)+"subagents"+string(filepath.Separator))
}

// Pricing variables (per million tokens, USD). Override via SetPricing.
var (
	inputPricePerMTok      = 15.0
	outputPricePerMTok     = 75.0
	cacheWritePricePerMTok = 18.75
	cacheReadPricePerMTok  = 1.50
)

// SetPricing overrides the default token pricing (per million tokens, USD).
func SetPricing(input, output, cacheWrite, cacheRead float64) {
	inputPricePerMTok = input
	outputPricePerMTok = output
	cacheWritePricePerMTok = cacheWrite
	cacheReadPricePerMTok = cacheRead
}

// estimateCost calculates an approximate USD cost based on token usage.
func estimateCost(stats TokenStats) float64 {
	cost := float64(stats.InputTokens) / 1_000_000 * inputPricePerMTok
	cost += float64(stats.OutputTokens) / 1_000_000 * outputPricePerMTok
	cost += float64(stats.CacheCreationInputTokens) / 1_000_000 * cacheWritePricePerMTok
	cost += float64(stats.CacheReadInputTokens) / 1_000_000 * cacheReadPricePerMTok

	return cost
}
