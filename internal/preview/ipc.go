// Package preview provides file-based IPC between the main claude-deck process
// (list mode) and the preview subprocess (claude-deck --preview running in a
// tmux __preview__ window).
//
// Protocol: the list process writes the selected session ID as plain text to
// {DataDir}/preview-selection using an atomic rename.  The preview process
// watches the file via fsnotify and renders the corresponding JSONL log
// whenever the file changes.
//
// This mirrors the existing ratelimits and hook-event IPC patterns.
package preview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/session"
)

const selectionFileName = "preview-selection"

// selectionPath returns the absolute path to the preview selection file.
func selectionPath(dataDir string) string {
	return filepath.Join(dataDir, selectionFileName)
}

// WriteSelection atomically writes sessionID to the selection file.
// Uses a temp-file rename so the reader never sees a partial write.
func WriteSelection(dataDir string, sessionID session.DeckSessionID) error {
	path := selectionPath(dataDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sessionID), 0o644); err != nil {
		return fmt.Errorf("preview: write selection: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("preview: rename selection: %w", err)
	}
	return nil
}

// ReadSelection reads the currently stored session ID from the selection file.
// Returns an empty ID if the file does not exist.
func ReadSelection(dataDir string) (session.DeckSessionID, error) {
	data, err := os.ReadFile(selectionPath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("preview: read selection: %w", err)
	}
	return session.DeckSessionID(strings.TrimSpace(string(data))), nil
}

// WatchSelection monitors the selection file via fsnotify and calls onChange
// whenever the selected session ID changes.  Blocks in a background goroutine
// until ctx is cancelled.
//
// If the file does not exist yet, the parent directory is watched and monitoring
// switches to the file once it appears — identical to the ratelimits pattern.
func WatchSelection(ctx context.Context, dataDir string, onChange func(session.DeckSessionID)) error {
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
					sid, err := ReadSelection(dataDir)
					if err != nil {
						debuglog.Printf("[preview] read selection error: %v", err)
						continue
					}
					if sid != "" {
						debuglog.Printf("[preview] selection changed: %s", sid)
						onChange(sid)
					}
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
