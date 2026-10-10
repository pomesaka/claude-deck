// Package usage reads Claude Code's local JSONL session logs
// from ~/.claude/projects/ and aggregates token usage per session.
package usage

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SessionInfo holds session metadata extracted from a Claude Code JSONL file.
// This is the primary data source; claude-deck's store only holds supplementary metadata.
type SessionInfo struct {
	SessionID      string
	CWD            string
	PermissionMode string
	GitBranch      string
	Prompt         string // first user message content
	StartedAt      time.Time
	LastActivity   time.Time
}

// Reader reads local JSONL session transcripts.
type Reader struct {
	baseDir string
	format  format
}

// NewReader creates a Reader. If baseDir is empty, defaults to ~/.claude/projects/.
func NewReader(baseDir string) *Reader {
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".claude", "projects")
	}
	return &Reader{baseDir: baseDir, format: claudeFormat{}}
}

// NewCodexReader creates a Reader for Codex CLI transcripts.
func NewCodexReader(baseDir string) *Reader {
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".codex", "sessions")
	}
	return &Reader{baseDir: baseDir, format: codexFormat{}}
}

// BaseDir returns the base directory for Claude Code JSONL files.
func (r *Reader) BaseDir() string {
	return r.baseDir
}

// ReadSessionInfoByID reads full session metadata for a specific Claude Code session.
func (r *Reader) ReadSessionInfoByID(sessionID string) *SessionInfo {
	path := r.ResolveSessionPath(sessionID)
	if path == "" {
		return nil
	}
	return r.format.info(path)
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

	marker := r.format.userMessageMarker()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)
	for scanner.Scan() {
		if bytes.Contains(scanner.Bytes(), marker) {
			return true
		}
	}
	return false
}

// ReadRuntimeActivity reads what the runtime is doing from the tail of its transcript.
func (r *Reader) ReadRuntimeActivity(path string) RuntimeActivity {
	return r.format.runtimeActivity(path)
}

// NewLogStreamer creates a streamer for a transcript of this Reader's runtime.
func (r *Reader) NewLogStreamer(path string) *LogStreamer {
	return newLogStreamer(r.format, path)
}

// NewMultiWatcher creates a MultiWatcher for the transcripts under this Reader's
// base directory. refreshInterval controls how often the watch list is re-evaluated.
func (r *Reader) NewMultiWatcher(refreshInterval time.Duration) (*MultiWatcher, error) {
	return newMultiWatcher(r.baseDir, r.format, refreshInterval)
}

// ListAllSessions returns SessionInfo for JSONL files in the projects directory.
// Subagent files (under subagents/) are excluded.
// Sessions with no activity in the last maxAge are skipped (0 means no limit).
// limit controls the maximum number of sessions returned (0 means no limit);
// offset skips the first N eligible sessions (for pagination).
// files are sorted by mtime descending so the most recent sessions are processed first.
// 軽量スキャン: 各ファイルの先頭数エントリだけ読み、mtime を LastActivity として使う。
func (r *Reader) ListAllSessions(maxAge time.Duration, limit, offset int) []*SessionInfo {
	jsonlFiles := r.format.files(r.baseDir)

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
		info := r.format.quickInfo(fe.path, fe.mtime)
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

// maxLineBytes is the longest JSONL line the readers accept. A longer line ends the read.
const maxLineBytes = 10 * 1024 * 1024

// scanLines calls fn with each line of r until fn returns false.
// WHY 行ごとに読む: jsontext.Decoder で値を続けて読むと、1 つの値のデコードに失敗した後は
// デコーダーが同じ位置でエラーを返し続ける（Go 1.26.0 の encoding/json/v2 で確認）。
// 失敗を飛ばして続けるループは終わらなくなる。行ごとなら、壊れた行だけを飛ばせる。
func scanLines(r io.Reader, fn func(line []byte) (more bool)) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for scanner.Scan() {
		if !fn(scanner.Bytes()) {
			return
		}
	}
}

// isSubagentPath returns true if the path is inside a "subagents" directory.
func isSubagentPath(path string) bool {
	return strings.Contains(path, string(filepath.Separator)+"subagents"+string(filepath.Separator))
}
