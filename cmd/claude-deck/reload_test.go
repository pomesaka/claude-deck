package main

import (
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pomesaka/claude-deck/internal/instance"
)

// fakeBuild writes a binary that answers `version` as a build does, or fails.
func fakeBuild(t *testing.T, works bool) string {
	t.Helper()
	body := `[ "$1" = version ] && echo '{"revision":"abc"}'`
	if !works {
		body = "echo broken; exit 2"
	}
	p := filepath.Join(t.TempDir(), "claude-deck")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReloadTUI_NoTUI(t *testing.T) {
	if _, err := reloadTUI(t.TempDir(), true); !errors.Is(err, errNoTUI) {
		t.Errorf("reloadTUI() = %v, want errNoTUI", err)
	}
}

func TestReloadTUI_DryRun(t *testing.T) {
	tests := []struct {
		name    string
		works   bool
		wantErr bool
	}{
		{"a working build", true, false},
		{"a broken build", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, bin := t.TempDir(), fakeBuild(t, tt.works)
			in, err := instance.Acquire(dir, bin, "abc")
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			info, err := reloadTUI(dir, true)
			if (err != nil) != tt.wantErr {
				t.Fatalf("reloadTUI() error = %v, want error %v", err, tt.wantErr)
			}
			if info.PID != os.Getpid() {
				t.Errorf("PID = %d, want this process", info.PID)
			}
			if info.Reloaded {
				t.Error("a dry run reloaded")
			}
			if !tt.wantErr && (info.Version == nil || info.Version.Revision != "abc") {
				t.Errorf("Version = %+v, want the build's revision abc", info.Version)
			}
		})
	}
}

// The test process stands in for the TUI: it answers the signal by starting
// again (a fresh Acquire), by refusing, or not at all.
func TestReloadTUI_Signal(t *testing.T) {
	old := reloadWait
	reloadWait = time.Second
	t.Cleanup(func() { reloadWait = old })
	tests := []struct {
		name         string
		answer       func(dir string, in *instance.Instance) error // what the "TUI" does on the signal
		wantReloaded bool
		wantErr      string
	}{
		{"the TUI starts again", func(dir string, in *instance.Instance) error {
			in.Close()
			_, err := instance.Acquire(dir, "/bin/claude-deck", "new")
			return err
		}, true, ""},
		{"the TUI refuses", func(_ string, in *instance.Instance) error {
			return in.RecordReloadError(errors.New("version: exit status 2"))
		}, false, "exit status 2"},
		{"the TUI does not answer", func(string, *instance.Instance) error { return nil }, false, "did not start again"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			in, err := instance.Acquire(dir, fakeBuild(t, true), "old")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { in.Close() })
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, reloadSignal)
			defer signal.Stop(ch)
			answered := make(chan error, 1)
			go func() { <-ch; answered <- tt.answer(dir, in) }()
			info, err := reloadTUI(dir, false)
			if aerr := <-answered; aerr != nil {
				t.Fatalf("the stand-in TUI: %v", aerr)
			}
			if info.Reloaded != tt.wantReloaded {
				t.Errorf("Reloaded = %v, want %v", info.Reloaded, tt.wantReloaded)
			}
			if tt.wantErr == "" && err != nil {
				t.Errorf("reloadTUI() = %v, want nil", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("reloadTUI() = %v, want an error saying %q", err, tt.wantErr)
			}
			if tt.wantReloaded && info.TUIRevision != "new" {
				t.Errorf("TUIRevision = %q, want the new TUI's", info.TUIRevision)
			}
		})
	}
}

// What a reload carries reaches the new process once, and is dropped after.
func TestReloadCarry(t *testing.T) {
	env := reloadEnv([]string{"PATH=/bin", envReloadCursor + "=old"}, reloadCarry{command: "/usr/local/bin/claude-deck", cursor: "abc", ghosttySplit: "uuid-1"})
	want := []string{"PATH=/bin", envReloaded + "=1", envReloadCommand + "=/usr/local/bin/claude-deck", envReloadCursor + "=abc", envReloadGhostty + "=uuid-1"}
	if !slices.Equal(env, want) {
		t.Errorf("reloadEnv() = %v, want %v", env, want)
	}
	for _, kv := range want[1:] {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	if c := takeReloadCarry(); !c.reloaded || c.command != "/usr/local/bin/claude-deck" || c.cursor != "abc" || c.ghosttySplit != "uuid-1" {
		t.Errorf("takeReloadCarry() = %+v", c)
	}
	if c := takeReloadCarry(); c.reloaded || c.cursor != "" {
		t.Errorf("a second takeReloadCarry() = %+v, want nothing", c)
	}
}
