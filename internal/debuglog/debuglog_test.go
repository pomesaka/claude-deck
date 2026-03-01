package debuglog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reset clears the package-level state between tests.
func reset() {
	Close()
}

func TestInit_Disabled(t *testing.T) {
	reset()
	t.Setenv("CLAUDE_DECK_DEBUG", "")

	if err := Init(); err != nil {
		t.Fatal(err)
	}
	if Enabled() {
		t.Error("expected disabled when CLAUDE_DECK_DEBUG is empty")
	}

	// Printf should be a safe no-op
	Printf("should not panic %d", 42)
}

func TestInit_BooleanValue(t *testing.T) {
	reset()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("CLAUDE_DECK_DEBUG", "1")

	if err := Init(); err != nil {
		t.Fatal(err)
	}
	defer reset()

	if !Enabled() {
		t.Fatal("expected enabled")
	}

	Printf("hello %s", "world")
	Close()

	logPath := filepath.Join(dir, "claude-deck", "debug.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("log file not created: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "[DEBUG] === debug log started") {
		t.Errorf("missing start marker, got:\n%s", content)
	}
	if !strings.Contains(content, "[DEBUG] hello world") {
		t.Errorf("missing hello world, got:\n%s", content)
	}
}

func TestInit_TrueValue(t *testing.T) {
	reset()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("CLAUDE_DECK_DEBUG", "true")

	if err := Init(); err != nil {
		t.Fatal(err)
	}
	defer reset()

	if !Enabled() {
		t.Fatal("expected enabled for 'true'")
	}
	Close()

	logPath := filepath.Join(dir, "claude-deck", "debug.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("log file not created for 'true': %v", err)
	}
}

func TestInit_CustomPath(t *testing.T) {
	reset()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sub", "custom.log")
	t.Setenv("CLAUDE_DECK_DEBUG", logPath)

	if err := Init(); err != nil {
		t.Fatal(err)
	}
	defer reset()

	if !Enabled() {
		t.Fatal("expected enabled")
	}

	Printf("custom path test")
	Close()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("custom log file not created: %v", err)
	}
	if !strings.Contains(string(data), "custom path test") {
		t.Errorf("missing message in custom log")
	}
}

func TestPrintf_Format(t *testing.T) {
	reset()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "fmt.log")
	t.Setenv("CLAUDE_DECK_DEBUG", logPath)

	if err := Init(); err != nil {
		t.Fatal(err)
	}
	defer reset()

	Printf("count=%d name=%s", 3, "test")
	Close()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	// Find the line with our message (skip the start marker line)
	var found string
	for _, l := range lines {
		if strings.Contains(l, "count=3") {
			found = l
			break
		}
	}
	if found == "" {
		t.Fatalf("message not found in output:\n%s", string(data))
	}

	// Verify timestamp format: 2006-01-02T15:04:05.000 [DEBUG] ...
	if !strings.Contains(found, "T") || !strings.Contains(found, "[DEBUG]") {
		t.Errorf("unexpected format: %s", found)
	}
	if !strings.Contains(found, "count=3 name=test") {
		t.Errorf("unexpected content: %s", found)
	}
}

func TestClose_Idempotent(t *testing.T) {
	reset()
	// Close on already-closed state should not panic
	Close()
	Close()
}
