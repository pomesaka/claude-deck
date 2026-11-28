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
	r := &tmux.Runner{Socket: fmt.Sprintf("cdsend-%d-%d", os.Getpid(), time.Now().UnixNano()), SessionName: "deck"}
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

func sendStore(t *testing.T, r store.Record) *store.Store {
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

func TestSendStored_Sends(t *testing.T) {
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
			st := sendStore(t, tt.rec)
			r := startTmux(t)
			typed := fakePrompt(t, r, "abc", tt.prompt)
			s, sent, err := SendStored(st, r, "", "anna-8cc7", "/clear", tt.dryRun)
			if err != nil || sent != tt.wantSent || s.ID != "abc" {
				t.Fatalf("SendStored() = (%s, %v, %v), want (abc, %v, nil)", s.ID, sent, err, tt.wantSent)
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

func TestSendStored_Refuses(t *testing.T) {
	tests := []struct {
		name   string
		status string
		prompt string
		self   DeckSessionID
		want   error
	}{
		{"running", "running", "❯ ", "", ErrSendBusy},
		{"half-typed text", "idle", "❯ fix the", "", ErrSendHalfTyped},
		{"this session", "idle", "❯ ", "abc", ErrSendSelf},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := sendStore(t, store.Record{Status: tt.status})
			r := startTmux(t)
			typed := fakePrompt(t, r, "abc", tt.prompt)
			if _, sent, err := SendStored(st, r, tt.self, "anna-8cc7", "/clear", false); !errors.Is(err, tt.want) || sent {
				t.Fatalf("SendStored() = (%v, %v), want (false, %v)", sent, err, tt.want)
			}
			if got := typedInto(typed, 300*time.Millisecond); got != "" {
				t.Errorf("the window received %q, want nothing", got)
			}
		})
	}
}

func TestSendStored_NoWindow(t *testing.T) {
	st := sendStore(t, store.Record{Status: "idle"})
	if _, sent, err := SendStored(st, startTmux(t), "", "abc", "/clear", false); err == nil || sent {
		t.Errorf("SendStored() = (%v, %v), want an error", sent, err)
	}
}

// The text is typed as it is: tmux key names and non-ASCII text arrive unchanged.
func TestSendStored_TypesTextAsIs(t *testing.T) {
	for _, text := range []string{"続けて", "Enter C-c /reload-plugins"} {
		t.Run(text, func(t *testing.T) {
			st := sendStore(t, store.Record{Status: "idle"})
			r := startTmux(t)
			typed := fakePrompt(t, r, "abc", "❯ ")
			if _, sent, err := SendStored(st, r, "", "abc", text, false); err != nil || !sent {
				t.Fatalf("SendStored() = (%v, %v), want sent", sent, err)
			}
			if got := typedInto(typed, 2*time.Second); got != text {
				t.Errorf("the window received %q, want %q", got, text)
			}
		})
	}
}

// Broadcast sends to the sessions that accept input and reports why it skipped the others.
func TestBroadcast(t *testing.T) {
	r := startTmux(t)
	targets := []struct {
		snap   Snapshot
		prompt string
	}{
		{Snapshot{ID: "idle1", Name: "idle1", Origin: OriginDeck, Status: StatusIdle}, "❯ "},
		{Snapshot{ID: "busy", Name: "busy", Origin: OriginDeck, Status: StatusRunning}, "❯ "},
		{Snapshot{ID: "typing", Name: "typing", Origin: OriginDeck, Status: StatusIdle}, "❯ draft"},
		{Snapshot{ID: "self", Name: "self", Origin: OriginDeck, Status: StatusIdle}, "❯ "},
	}
	typed := map[DeckSessionID]string{}
	var snaps []Snapshot
	for _, tg := range targets {
		typed[tg.snap.ID] = fakePrompt(t, r, tg.snap.ID, tg.prompt)
		snaps = append(snaps, tg.snap)
	}
	tests := []struct {
		name   string
		dryRun bool
		want   []error // Skipped of each target, in order
		sent   []bool
	}{
		{"dry run", true, []error{nil, ErrSendBusy, ErrSendHalfTyped, ErrSendSelf}, []bool{false, false, false, false}},
		{"send", false, []error{nil, ErrSendBusy, ErrSendHalfTyped, ErrSendSelf}, []bool{true, false, false, false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Broadcast(r, snaps, "self", "/reload-plugins", tt.dryRun)
			if err != nil || len(got) != len(snaps) {
				t.Fatalf("Broadcast() = (%d deliveries, %v), want %d", len(got), err, len(snaps))
			}
			for i, d := range got {
				if d.Session.ID != snaps[i].ID || d.Sent != tt.sent[i] || !errors.Is(d.Skipped, tt.want[i]) || (tt.want[i] == nil && d.Skipped != nil) {
					t.Errorf("delivery %d = {%s, sent %v, %v}, want {%s, sent %v, %v}", i, d.Session.ID, d.Sent, d.Skipped, snaps[i].ID, tt.sent[i], tt.want[i])
				}
			}
		})
	}
	if got := typedInto(typed["idle1"], 2*time.Second); got != "/reload-plugins" {
		t.Errorf("idle1 received %q, want /reload-plugins", got)
	}
	for _, id := range []DeckSessionID{"busy", "typing", "self"} {
		if got := typedInto(typed[id], 300*time.Millisecond); got != "" {
			t.Errorf("%s received %q, want nothing", id, got)
		}
	}
}

func TestBroadcast_RefusesTheText(t *testing.T) {
	if _, err := Broadcast(&tmux.Runner{}, nil, "", "a\nb", false); !errors.Is(err, ErrSendMultiline) {
		t.Errorf("Broadcast() = %v, want ErrSendMultiline", err)
	}
}
