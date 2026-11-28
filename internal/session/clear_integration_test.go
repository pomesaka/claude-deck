package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pomesaka/claude-deck/internal/store"
	"github.com/pomesaka/claude-deck/internal/tmux"
)

// startTmux runs a tmux server of the test's own (tmux -L) and kills it when the test ends.
func startTmux(t *testing.T) *tmux.Runner {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	r := &tmux.Runner{Socket: fmt.Sprintf("cdclear-%d-%d", os.Getpid(), time.Now().UnixNano()), SessionName: "deck"}
	if out, err := exec.Command("tmux", "-L", r.Socket, "new-session", "-d", "-s", r.SessionName, "-x", "120", "-y", "20", "sleep 600").CombinedOutput(); err != nil {
		t.Fatalf("starting tmux: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", r.Socket, "kill-server").Run() })
	return r
}

// fakePrompt opens a window that draws prompt as Claude Code's input box and
// writes the next line typed into it to the returned file.
func fakePrompt(t *testing.T, r *tmux.Runner, window DeckSessionID, prompt string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "typed")
	// printf reads \033 as the escape; the prompts here hold no single quote or %.
	cmd := fmt.Sprintf(`printf '%s\n'; read l; printf %%s "$l" > %q; sleep 600`, strings.ReplaceAll(prompt, "\x1b", `\033`), out)
	if err := r.NewWindow(string(window), tmux.WindowOpts{Command: cmd}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if screen, _ := r.CapturePane(string(window)); strings.Contains(screen, "❯") {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatal("the window never drew its prompt")
		}
	}
}

// typedInto waits up to wait for the line the window received.
func typedInto(path string, wait time.Duration) string {
	for deadline := time.Now().Add(wait); ; time.Sleep(20 * time.Millisecond) {
		if got, _ := os.ReadFile(path); len(got) > 0 || time.Now().After(deadline) {
			return string(got)
		}
	}
}

func clearStore(t *testing.T, r store.Record) *store.Store {
	t.Helper()
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r.ID, r.Name, r.Isolation = "abc", "anna-8cc7", store.IsolationDirect
	if err := st.Insert(r); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestClearStored_Sends(t *testing.T) {
	tests := []struct {
		name     string
		rec      store.Record
		prompt   string
		dryRun   bool
		wantSent bool
		want     string // what the window received
	}{
		{"idle with an empty prompt", store.Record{Status: "idle"}, "❯ ", false, true, "/clear"},
		{"waiting_action with only a suggestion", store.Record{Status: "waiting_action", StatusReason: "x", WaitKind: "command"}, "❯ \x1b[2mtry this\x1b[0m", false, true, "/clear"},
		{"dry run sends nothing", store.Record{Status: "idle"}, "❯ ", true, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := clearStore(t, tt.rec)
			r := startTmux(t)
			typed := fakePrompt(t, r, "abc", tt.prompt)
			s, sent, err := ClearStored(st, r, "", "anna-8cc7", tt.dryRun)
			if err != nil || sent != tt.wantSent || s.ID != "abc" {
				t.Fatalf("ClearStored() = (%s, %v, %v), want (abc, %v, nil)", s.ID, sent, err, tt.wantSent)
			}
			wait := 2 * time.Second
			if tt.want == "" {
				wait = 300 * time.Millisecond // nothing should arrive
			}
			if got := typedInto(typed, wait); got != tt.want {
				t.Errorf("the window received %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClearStored_Refuses(t *testing.T) {
	tests := []struct {
		name   string
		status string
		prompt string
		self   DeckSessionID
		want   error
	}{
		{"running", "running", "❯ ", "", ErrClearBusy},
		{"half-typed text", "idle", "❯ fix the", "", ErrClearHalfTyped},
		{"this session", "idle", "❯ ", "abc", ErrClearSelf},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := clearStore(t, store.Record{Status: tt.status})
			r := startTmux(t)
			typed := fakePrompt(t, r, "abc", tt.prompt)
			if _, sent, err := ClearStored(st, r, tt.self, "anna-8cc7", false); !errors.Is(err, tt.want) || sent {
				t.Fatalf("ClearStored() = (%v, %v), want (false, %v)", sent, err, tt.want)
			}
			if got := typedInto(typed, 300*time.Millisecond); got != "" {
				t.Errorf("the window received %q, want nothing", got)
			}
		})
	}
}

func TestClearStored_NoWindow(t *testing.T) {
	st := clearStore(t, store.Record{Status: "idle"})
	if _, sent, err := ClearStored(st, startTmux(t), "", "abc", false); err == nil || sent {
		t.Errorf("ClearStored() = (%v, %v), want an error", sent, err)
	}
}
