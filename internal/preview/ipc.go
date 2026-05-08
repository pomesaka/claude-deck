// Package preview provides file-based IPC between the main claude-deck process
// (list mode) and the preview subprocess (claude-deck --preview running in a
// tmux __preview__ window).
//
// Protocol: the list process writes a JSON PreviewSpec to {DataDir}/preview-selection
// using an atomic rename.  The preview process watches the file via fsnotify and
// renders the corresponding JSONL log whenever the file changes.
//
// This mirrors the existing ratelimits and hook-event IPC patterns.
package preview

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/session"
)

const selectionFileName = "preview-selection"

// PreviewSpec は main プロセスから preview サブプロセスへ渡す描画仕様。
// DeckSessionID は内部トレース用（preview 側のルックアップキーとしては使用しない）。
// Display が空文字列の場合は「選択なし」を意味する。
type PreviewSpec struct {
	DeckSessionID   session.DeckSessionID     `json:"deck_session_id"`
	Name            string                    `json:"name"`
	RepoName        string                    `json:"repo_name"`
	WorkspacePath   string                    `json:"workspace_path"`
	ClaudeSessionID session.ClaudeSessionID   `json:"claude_session_id"`
	PriorClaudeIDs  []session.ClaudeSessionID `json:"prior_claude_ids"`
	ClearCount      int                       `json:"clear_count"`
	Status          string                    `json:"status"`
	Display         string                    `json:"display"` // "jsonl" | "tmux" | ""
	CurrentTool     string                    `json:"current_tool"`
	ErrorMessage    string                    `json:"error_message"`
	NeedsAttention  bool                      `json:"needs_attention"`
	// JSONL パスは main プロセス側で解決済み。preview は claude projects を探索しない。
	JSONLPath       string   `json:"jsonl_path"`
	PriorJSONLPaths []string `json:"prior_jsonl_paths"` // /clear 履歴（古い順）
}

// selectionPath returns the absolute path to the preview selection file.
func selectionPath(dataDir string) string {
	return filepath.Join(dataDir, selectionFileName)
}

// WriteSpec atomically writes spec to the selection file as JSON.
// Uses a temp-file rename so the reader never sees a partial write.
func WriteSpec(dataDir string, spec PreviewSpec) error {
	data, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("preview: marshal spec: %w", err)
	}
	path := selectionPath(dataDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("preview: write spec: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("preview: rename spec: %w", err)
	}
	return nil
}

// ReadSpec reads the currently stored PreviewSpec from the selection file.
// Returns a zero-value PreviewSpec (Display=="") if the file does not exist.
func ReadSpec(dataDir string) (PreviewSpec, error) {
	data, err := os.ReadFile(selectionPath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return PreviewSpec{}, nil
		}
		return PreviewSpec{}, fmt.Errorf("preview: read spec: %w", err)
	}
	var spec PreviewSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return PreviewSpec{}, fmt.Errorf("preview: unmarshal spec: %w", err)
	}
	return spec, nil
}

// WatchSpec monitors the selection file via fsnotify and calls onChange
// whenever the spec changes.  Blocks in a background goroutine
// until ctx is cancelled.
//
// If the file does not exist yet, the parent directory is watched and monitoring
// switches to the file once it appears — identical to the ratelimits pattern.
//
// Note: watcher errors are logged but not propagated; the goroutine exits silently
// if the watcher channel closes unexpectedly. Callers cannot detect this condition.
func WatchSpec(ctx context.Context, dataDir string, onChange func(PreviewSpec)) error {
	path := selectionPath(dataDir)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("preview: create watcher: %w", err)
	}

	watchTarget := path
	if _, err := os.Stat(path); os.IsNotExist(err) {
		watchTarget = dataDir
	}
	if err := watcher.Add(watchTarget); err != nil {
		watcher.Close()
		return fmt.Errorf("preview: watch %s: %w", watchTarget, err)
	}

	go func() {
		defer watcher.Close()
		watchingDir := watchTarget == dataDir

		for {
			select {
			case <-ctx.Done():
				return

			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				isOurFile := filepath.Clean(event.Name) == filepath.Clean(path)

				// Switch from dir-watch to file-watch once the file appears.
				if watchingDir && isOurFile && (event.Has(fsnotify.Create) || event.Has(fsnotify.Write)) {
					if err := watcher.Add(path); err == nil {
						_ = watcher.Remove(dataDir)
						watchingDir = false
						debuglog.Printf("[preview] switched to watching file %s", path)
					}
				}

				if isOurFile && (event.Has(fsnotify.Write) || event.Has(fsnotify.Create)) {
					spec, err := ReadSpec(dataDir)
					if err != nil {
						debuglog.Printf("[preview] read spec error: %v", err)
						continue
					}
					debuglog.Printf("[preview] spec changed: deck=%s display=%s", spec.DeckSessionID, spec.Display)
					onChange(spec)
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				debuglog.Printf("[preview] watcher error: %v", err)
			}
		}
	}()

	return nil
}
