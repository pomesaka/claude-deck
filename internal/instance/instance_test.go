package instance

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFind(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := Find(dir); ok || err != nil {
		t.Fatalf("Find() with no file = (%v, %v), want not running", ok, err)
	}
	in, err := Acquire(dir, "/bin/claude-deck", "")
	if err != nil {
		t.Fatal(err)
	}
	st, ok, err := Find(dir)
	if !ok || err != nil || st.PID != os.Getpid() || st.Command != "/bin/claude-deck" {
		t.Fatalf("Find() while held = (%+v, %v, %v), want this process", st, ok, err)
	}
	if _, err := Acquire(dir, "/bin/claude-deck", ""); err != ErrRunning {
		t.Errorf("a second Acquire() = %v, want ErrRunning", err)
	}
	if err := in.RecordReloadError(os.ErrPermission); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := Find(dir); st.ReloadError == "" || st.ReloadErrorAt == nil {
		t.Errorf("after RecordReloadError: %+v, want the reason and its time", st)
	}
	in.Close()
	// The file stays, with the old pid in it; nobody holds it.
	if _, ok, err := Find(dir); ok || err != nil {
		t.Errorf("Find() after Close = (%v, %v), want not running", ok, err)
	}
	again, err := Acquire(dir, "/bin/new", "")
	if err != nil {
		t.Fatalf("Acquire() after Close: %v", err)
	}
	defer again.Close()
	if st, _, _ := Find(dir); st.Command != "/bin/new" || st.ReloadError != "" {
		t.Errorf("after a new Acquire: %+v, want a fresh state", st)
	}
}

func TestCheckBinary(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := checkTimeout
	checkTimeout = 500 * time.Millisecond
	t.Cleanup(func() { checkTimeout = old })
	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{"a working build", script("ok", `[ "$1" = version ] && echo '{"revision":"abc"}'`), `{"revision":"abc"}`, false},
		{"a build that fails", script("fail", "echo boom; exit 2"), "", true},
		{"a build that hangs", script("hang", "exec sleep 5"), "", true},
		{"no file", filepath.Join(dir, "missing"), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CheckBinary(tt.path)
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("CheckBinary() = (%q, %v), want (%q, error %v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// A reload follows the link the TUI was started from as it is now.
func TestTarget(t *testing.T) {
	dir := t.TempDir()
	oldBuild, newBuild, link := filepath.Join(dir, "old"), filepath.Join(dir, "new"), filepath.Join(dir, "claude-deck")
	for _, p := range []string{oldBuild, newBuild} {
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(oldBuild, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(newBuild, link); err != nil {
		t.Fatal(err)
	}
	got, err := Target(link)
	want, _ := filepath.EvalSymlinks(newBuild)
	if err != nil || got != want {
		t.Errorf("Target() = (%q, %v), want %q", got, err, want)
	}
}
