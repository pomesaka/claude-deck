// Package hooks manages Claude Code hook auto-configuration and event file watching.
//
// Claude Code の /clear 実行時:
//  1. SessionEnd  {session_id: OLD, reason: "clear"} が発火
//  2. SessionStart {session_id: NEW, source: "clear"} が発火
//
// この2つをペアリングすることで OLD → NEW の session ID 紐付けを行い、
// claude-deck の ClaudeSessionID を更新する。
package hooks

import (
	"bufio"
	"context"
	json "encoding/json/v2"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/pomesaka/sandbox/claude-deck/internal/debuglog"
)

// Event represents a hook event written by Claude Code.
type Event struct {
	SessionID           string `json:"session_id"`
	CWD                 string `json:"cwd"`
	HookEventName       string `json:"hook_event_name"` // "SessionStart", "SessionEnd", "Notification", "Stop"
	Source              string `json:"source,omitempty"` // SessionStart: "startup", "resume", "clear", "compact"
	Reason              string `json:"reason,omitempty"` // SessionEnd: "clear", "logout", etc.
	NotificationType    string `json:"notification_type,omitempty"` // Notification: "permission_prompt", "elicitation_dialog", "idle_prompt"
	ClaudeDeckSessionID string `json:"claude_deck_session_id,omitempty"` // PTY 起動時に環境変数から注入
}

// EventsFileName is the basename of the events JSONL file.
const EventsFileName = "claude-deck-events.jsonl"

// EventsFilePath returns the full path to the events file under dataDir.
func EventsFilePath(dataDir string) string {
	return filepath.Join(dataDir, EventsFileName)
}

// settingsPath returns the path to Claude Code's global settings file.
func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// hookMarker is used to identify legacy hooks in settings.json.
const hookMarker = "claude-deck-events"

// HookStatus describes the state of hook configuration.
type HookStatus int

const (
	// HookStatusNone means no hooks are configured (new user).
	HookStatusNone HookStatus = iota
	// HookStatusLegacy means hooks are injected in ~/.claude/settings.json (pre-plugin).
	HookStatusLegacy
	// HookStatusPlugin means legacy hooks are absent (assumed plugin-managed).
	HookStatusPlugin
)

// CheckHooks inspects ~/.claude/settings.json for legacy claude-deck hooks.
// Returns the hook status without modifying any files.
func CheckHooks() HookStatus {
	path, err := settingsPath()
	if err != nil {
		return HookStatusNone
	}

	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return HookStatusNone
	}

	if strings.Contains(string(data), hookMarker) {
		return HookStatusLegacy
	}

	return HookStatusPlugin
}

// RemoveLegacyHooks removes claude-deck hooks from ~/.claude/settings.json.
// Call this after the user has installed the plugin to clean up.
func RemoveLegacyHooks() error {
	path, err := settingsPath()
	if err != nil {
		return fmt.Errorf("resolving settings path: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading settings: %w", err)
	}

	var settings map[string]jsontext.Value
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("parsing settings: %w", err)
	}

	var hooksMap map[string]jsontext.Value
	if raw, ok := settings["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooksMap); err != nil {
			return fmt.Errorf("parsing hooks: %w", err)
		}
	}
	if hooksMap == nil {
		return nil
	}

	changed := false
	for eventName, raw := range hooksMap {
		var entries []jsontext.Value
		if err := json.Unmarshal(raw, &entries); err != nil {
			continue
		}
		cleaned := removeEntriesWithMarker(entries, hookMarker)
		if len(cleaned) != len(entries) {
			changed = true
			if len(cleaned) == 0 {
				delete(hooksMap, eventName)
			} else {
				entriesJSON, _ := json.Marshal(cleaned)
				hooksMap[eventName] = jsontext.Value(entriesJSON)
			}
		}
	}

	if !changed {
		return nil
	}

	if len(hooksMap) == 0 {
		delete(settings, "hooks")
	} else {
		hooksJSON, _ := json.Marshal(hooksMap)
		settings["hooks"] = jsontext.Value(hooksJSON)
	}

	out, err := json.Marshal(settings, jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Errorf("marshaling settings: %w", err)
	}

	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("writing settings: %w", err)
	}

	debuglog.Printf("[hooks] removed legacy hooks from settings.json")
	return nil
}

// removeEntriesWithMarker filters out entries whose JSON contains the marker.
func removeEntriesWithMarker(entries []jsontext.Value, marker string) []jsontext.Value {
	var result []jsontext.Value
	for _, e := range entries {
		if !strings.Contains(string(e), marker) {
			result = append(result, e)
		}
	}
	return result
}

// WatchEvents watches the events JSONL file for new lines and calls onEvent
// for each parsed Event. Blocks until ctx is cancelled.
//
// fsnotify で Write イベントを検知し、前回の読み取り位置から新しい行を読む。
// ファイルが存在しない場合は作成を待つ。
func WatchEvents(ctx context.Context, eventsPath string, onEvent func(Event)) error {
	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(eventsPath), 0o755); err != nil {
		return fmt.Errorf("creating events dir: %w", err)
	}

	// Create the file if it doesn't exist
	f, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening events file: %w", err)
	}

	// Seek to end (only process new events)
	offset, err := f.Seek(0, 2)
	if err != nil {
		f.Close()
		return fmt.Errorf("seeking to end: %w", err)
	}
	f.Close()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}

	if err := watcher.Add(eventsPath); err != nil {
		watcher.Close()
		return fmt.Errorf("watching events file: %w", err)
	}

	go func() {
		defer watcher.Close()

		for {
			select {
			case <-ctx.Done():
				return

			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if !event.Has(fsnotify.Write) {
					continue
				}
				debuglog.Printf("[hooks] fsnotify Write detected")
				offset = readNewLines(eventsPath, offset, onEvent)

			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			}
		}
	}()

	return nil
}

// readNewLines reads new JSONL lines from the given offset and returns the updated offset.
// ファイルがトランケートされた場合（サイズ < offset）は先頭からリセットする。
func readNewLines(path string, offset int64, onEvent func(Event)) int64 {
	f, err := os.Open(path)
	if err != nil {
		return offset
	}
	defer f.Close()

	// ファイルがトランケートされた場合、offset をリセット
	fi, err := f.Stat()
	if err != nil {
		return offset
	}
	if fi.Size() < offset {
		offset = 0
	}

	if _, err := f.Seek(offset, 0); err != nil {
		return offset
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			debuglog.Printf("[hooks] failed to parse event: %v (line: %s)", err, line)
			continue
		}
		if ev.SessionID != "" {
			debuglog.Printf("[hooks] event: %s session_id=%s cwd=%s source=%s reason=%s notification_type=%s",
				ev.HookEventName, ev.SessionID, ev.CWD, ev.Source, ev.Reason, ev.NotificationType)
			onEvent(ev)
		}
	}

	// Update offset to current position
	newOffset, err := f.Seek(0, 1)
	if err != nil {
		return offset
	}
	return newOffset
}

// TruncateEventsFile clears the events file. Called on startup to avoid
// processing stale events.
func TruncateEventsFile(eventsPath string) error {
	// ファイルが存在しなければ何もしない
	if _, err := os.Stat(eventsPath); os.IsNotExist(err) {
		return nil
	}
	return os.Truncate(eventsPath, 0)
}

// CleanupStaleEvents removes events older than maxAge from the events file.
// 定期的に呼んでファイルの肥大化を防ぐ。
func CleanupStaleEvents(eventsPath string, _ time.Duration) {
	fi, err := os.Stat(eventsPath)
	if err != nil || fi.Size() < 1024*1024 { // 1MB 未満ならスキップ
		return
	}
	// 単純にトランケート（イベントは即時処理するため古いデータは不要）
	_ = os.Truncate(eventsPath, 0)
}
