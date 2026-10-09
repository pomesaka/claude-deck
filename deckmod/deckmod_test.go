package deckmod

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInstall(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugin")
	got, err := Install(dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got != dir {
		t.Errorf("Install returned %q, want %q", got, dir)
	}
	for _, rel := range []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.ts", "skills/claude-deck/SKILL.md"} {
		want, err := files.ReadFile(rel)
		if err != nil {
			t.Fatalf("embedded %s: %v", rel, err)
		}
		gotBytes, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("installed %s: %v", rel, err)
		}
		if string(gotBytes) != string(want) {
			t.Errorf("%s differs from the embedded file", rel)
		}
	}
}

// A second install leaves matching files alone, so a running Claude Code does not
// see them rewritten (and hot-reload) every time claude-deck starts.
func TestInstall_LeavesMatchingFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := Install(dir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	path := filepath.Join(dir, "hooks", "register.ts")
	old := time.Unix(1_600_000_000, 0)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(dir); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Errorf("register.ts was rewritten (mtime %v)", info.ModTime())
	}
}

func TestInstall_ReplacesStaleFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks", "register.ts")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(dir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	got, _ := os.ReadFile(path)
	want, _ := files.ReadFile("hooks/register.ts")
	if string(got) != string(want) {
		t.Error("stale register.ts was not replaced")
	}
}
