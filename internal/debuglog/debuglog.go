// Package debuglog provides a simple debug logging facility controlled by
// the CLAUDE_DECK_DEBUG environment variable.
//
// Usage:
//
//	CLAUDE_DECK_DEBUG=1 ./claude-deck            # logs to $XDG_DATA_HOME/claude-deck/debug.log
//	CLAUDE_DECK_DEBUG=/tmp/my.log ./claude-deck  # logs to the specified path
//
// When the variable is unset or empty, all logging is silently discarded.
package debuglog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	mu sync.Mutex
	w  io.Writer // nil = disabled
	cl io.Closer // non-nil only when we opened a file
)

// Init reads CLAUDE_DECK_DEBUG and opens the log destination.
// Call once at startup; returns an error only if a file cannot be created.
func Init() error {
	val := os.Getenv("CLAUDE_DECK_DEBUG")
	if val == "" {
		return nil
	}

	var path string
	switch strings.ToLower(val) {
	case "1", "true":
		path = defaultLogPath()
	default:
		path = val
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("debuglog: mkdir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("debuglog: open: %w", err)
	}

	mu.Lock()
	w = f
	cl = f
	mu.Unlock()

	Printf("=== debug log started (pid %d) ===", os.Getpid())
	return nil
}

// Printf writes a timestamped line to the debug log.
// If logging is disabled, the call is a no-op.
func Printf(format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()

	if w == nil {
		return
	}

	ts := time.Now().Format("2006-01-02T15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(w, "%s [DEBUG] %s\n", ts, msg)
}

// Close flushes and closes the log file. Safe to call even when logging is disabled.
func Close() {
	mu.Lock()
	defer mu.Unlock()

	if cl != nil {
		cl.Close()
		cl = nil
	}
	w = nil
}

// Enabled returns whether debug logging is currently active.
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return w != nil
}

func defaultLogPath() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "claude-deck", "debug.log")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "claude-deck", "debug.log")
}
