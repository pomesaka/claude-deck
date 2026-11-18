package tui

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/pomesaka/claude-deck/internal/config"
	"github.com/pomesaka/claude-deck/internal/ratelimits"
	"github.com/pomesaka/claude-deck/internal/session"
)

// fakeManager serves a fixed list of sessions. The operations the TUI starts
// in Cmds are never run by these tests; their results are sent as msgs.
type fakeManager struct {
	sessions []*session.Session
}

func (f *fakeManager) GetSession(id session.DeckSessionID) *session.Session {
	for _, s := range f.sessions {
		if s.ID == id {
			return s
		}
	}
	return nil
}
func (f *fakeManager) ListSessions() []*session.Session { return f.sessions }
func (f *fakeManager) RefreshFromJSONL()                {}
func (f *fakeManager) ResolveJSONLPaths(session.DeckSessionID) (string, []string) {
	return "", nil
}
func (f *fakeManager) CreateSession(context.Context, string, string, bool) (*session.Session, error) {
	return nil, errors.New("not used")
}
func (f *fakeManager) ForkSession(context.Context, session.DeckSessionID) (*session.Session, error) {
	return nil, errors.New("not used")
}
func (f *fakeManager) ResumeSession(context.Context, session.DeckSessionID) error { return nil }
func (f *fakeManager) Kill(session.DeckSessionID) (session.CloseResult, error) {
	return session.CloseResult{}, nil
}
func (f *fakeManager) SetPinned(session.DeckSessionID, bool) error { return nil }

// dashboard drives a Model built from fakes: sessions named name-00, name-01, …
// in list order, and a right pane worker that records its I/O.
type dashboard struct {
	t    *testing.T
	m    Model
	mgr  *fakeManager
	pane *fakeRightPane
	quit bool
}

func newDashboard(t *testing.T, n, height int) *dashboard {
	t.Helper()
	mgr := &fakeManager{}
	for i := range n {
		mgr.sessions = append(mgr.sessions, newNamedSession(fmt.Sprintf("name-%02d", i)))
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := &dashboard{t: t, mgr: mgr, pane: newFakeRightPane(false)}
	d.m = newModel(mgr, config.Default(), ctx)
	d.m.rightPane = newRightPaneWorker(ctx, d.m.rightPaneGeneration, d.pane.ops())
	d.send(tea.WindowSizeMsg{Width: 80, Height: height})
	return d
}

func newNamedSession(name string) *session.Session {
	s := session.NewSession("/repo", "repo")
	s.Name = name
	return s
}

// send runs Update and keeps only whether it asked to quit. Cmds are not run:
// a test sends the msgs it wants explicitly.
func (d *dashboard) send(msg tea.Msg) {
	d.t.Helper()
	next, cmd := d.m.Update(msg)
	m, ok := next.(Model)
	if !ok {
		d.t.Fatalf("Update returned %T", next)
	}
	d.m = m
	d.quit = cmd != nil && isQuit(cmd)
}

// isQuit reports whether cmd is tea.Quit, alone or batched. Cmds that do not
// return at once (ticks) are given up on.
func isQuit(cmd tea.Cmd) bool {
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		switch msg := msg.(type) {
		case tea.QuitMsg:
			return true
		case tea.BatchMsg:
			return slices.ContainsFunc(msg, func(c tea.Cmd) bool { return c != nil && isQuit(c) })
		}
	case <-time.After(20 * time.Millisecond):
	}
	return false
}

func (d *dashboard) key(s string) {
	d.t.Helper()
	switch s {
	case "ctrl+c":
		d.send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	case "ctrl+z":
		d.send(tea.KeyPressMsg{Code: 'z', Mod: tea.ModCtrl})
	case "esc":
		d.send(tea.KeyPressMsg{Code: tea.KeyEscape})
	case "enter":
		d.send(tea.KeyPressMsg{Code: tea.KeyEnter})
	default:
		for _, r := range s {
			d.send(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}
}

func (d *dashboard) rateLimits(on bool) {
	var st ratelimits.Status
	if on {
		st = ratelimits.Status{FiveHourAvailable: true, FiveHour: ratelimits.Window{UsedPct: 40}}
	}
	d.send(RateLimitsUpdatedMsg{Status: st})
}

func (d *dashboard) selectedName() string {
	if d.m.cursor < 0 || d.m.cursor >= len(d.m.viewSnaps) {
		return ""
	}
	return d.m.viewSnaps[d.m.cursor].Name
}

// footer is the last line of the screen.
func (d *dashboard) footer() string {
	lines := strings.Split(d.m.View().Content, "\n")
	return lines[len(lines)-1]
}

// assertCursorDrawn fails unless the session under the cursor is drawn as a
// row of the list. It asks the view's mouse handler which session each screen
// row holds, because the header also shows the selected session's name.
func (d *dashboard) assertCursorDrawn() {
	d.t.Helper()
	if d.m.cursor < 0 || d.m.cursor >= len(d.m.viewSnaps) {
		return
	}
	id := d.m.viewSnaps[d.m.cursor].ID
	v := d.m.View()
	for y := range d.m.height {
		if clickedSession(v, tea.MouseLeft, y) == id {
			return
		}
	}
	d.t.Fatalf("cursor is on %s (index %d, offset %d) but its row is not drawn:\n%s",
		d.selectedName(), d.m.cursor, d.m.scrollOffset, v.Content)
}

// Whatever moves the cursor or narrows the list box, the session under the
// cursor stays inside the window the view draws.
func TestScrollToShow_CursorInsideWindow(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for range 5000 {
		height := 3 + rng.IntN(40)
		filterBar := rng.IntN(2)
		n := rng.IntN(40)
		cursor := 0
		if n > 0 {
			cursor = rng.IntN(n)
		}
		offset := rng.IntN(50)

		got := scrollToShow(height, filterBar, offset, cursor, n)
		if n == 0 {
			if got != 0 {
				t.Fatalf("n=0: offset = %d, want 0", got)
			}
			continue
		}
		w := sessionListWindow(height, filterBar, got, n)
		if cursor < w.start || cursor >= w.end {
			t.Fatalf("height=%d filterBar=%d offset=%d cursor=%d n=%d: window [%d,%d) misses the cursor",
				height, filterBar, offset, cursor, n, w.start, w.end)
		}
		// No blank space is left below when the window could show more from above.
		if got > 0 {
			if up := sessionListWindow(height, filterBar, got-1, n); up.end == n && cursor < up.end {
				t.Fatalf("height=%d n=%d cursor=%d: offset %d leaves room, %d also shows the cursor and the end", height, n, cursor, got, got-1)
			}
		}
	}
}

func TestView_CursorRowIsDrawn(t *testing.T) {
	tests := []struct {
		name  string
		n     int
		steps func(d *dashboard)
	}{
		{
			name: "rate limit row shown, j down to the last session",
			n:    20,
			steps: func(d *dashboard) {
				d.rateLimits(true)
				d.key("gg")
				for range 19 {
					d.key("j")
					d.assertCursorDrawn()
				}
			},
		},
		{
			name: "no rate limit row, j down to the last session",
			n:    20,
			steps: func(d *dashboard) {
				d.key("gg")
				for range 19 {
					d.key("j")
					d.assertCursorDrawn()
				}
			},
		},
		{
			name: "rate limit row shown, k up from the last session",
			n:    20,
			steps: func(d *dashboard) {
				d.rateLimits(true)
				d.key("G")
				for range 19 {
					d.key("k")
					d.assertCursorDrawn()
				}
			},
		},
		{
			name: "rate limit row appears while the cursor is on the last drawn row",
			n:    20,
			steps: func(d *dashboard) {
				d.key("G")
				d.rateLimits(true)
			},
		},
		{
			name: "filter bar opens while the cursor is on the last drawn row",
			n:    20,
			steps: func(d *dashboard) {
				d.rateLimits(true)
				d.key("G")
				d.key("/")
			},
		},
		{
			name: "window shrinks by one line",
			n:    20,
			steps: func(d *dashboard) {
				d.rateLimits(true)
				d.key("G")
				d.send(tea.WindowSizeMsg{Width: 80, Height: d.m.height - 1})
			},
		},
	}
	for _, tt := range tests {
		for _, height := range []int{12, 15, 16, 21, 30} {
			t.Run(fmt.Sprintf("%s/height=%d", tt.name, height), func(t *testing.T) {
				d := newDashboard(t, tt.n, height)
				tt.steps(d)
				d.assertCursorDrawn()
			})
		}
	}
}

// A resumed, created or forked session becomes the selected one, under the
// cursor and on the right pane, whatever the cursor and the filter were doing
// while the operation ran.
func TestUpdate_StartedSessionIsSelected(t *testing.T) {
	tests := []struct {
		name  string
		steps func(d *dashboard) tea.Msg
		want  string // the session that must be selected and on the right pane; "" = unchanged
	}{
		{
			name: "resume: the cursor moved away while it ran",
			steps: func(d *dashboard) tea.Msg {
				d.key("gg")
				id := d.m.selectedID
				d.key("j")
				return sessionResumedMsg{sessionID: id}
			},
			want: "name-00",
		},
		{
			name: "resume: the cursor stayed",
			steps: func(d *dashboard) tea.Msg {
				d.key("gg")
				return sessionResumedMsg{sessionID: d.m.selectedID}
			},
			want: "name-00",
		},
		{
			name: "create: a confirmed filter hides the new session",
			steps: func(d *dashboard) tea.Msg {
				d.key("/")
				d.key("name-01")
				d.key("enter")
				return sessionCreatedMsg{sessionID: d.addSession("new-one")}
			},
			want: "new-one",
		},
		{
			name: "create: a filter being typed hides the new session",
			steps: func(d *dashboard) tea.Msg {
				d.key("/")
				d.key("name-01")
				return sessionCreatedMsg{sessionID: d.addSession("new-one")}
			},
			want: "new-one",
		},
		{
			name: "fork: a confirmed filter hides the new session",
			steps: func(d *dashboard) tea.Msg {
				d.key("/")
				d.key("name-01")
				d.key("enter")
				return sessionForkedMsg{sessionID: d.addSession("new-one")}
			},
			want: "new-one",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDashboard(t, 3, 30)
			msg := tt.steps(d)
			d.send(msg)

			if d.m.filterActive || d.m.filterText != "" || d.m.filterInput.Value() != "" {
				t.Errorf("filter still set: active=%v text=%q input=%q", d.m.filterActive, d.m.filterText, d.m.filterInput.Value())
			}
			if got := d.selectedName(); got != tt.want {
				t.Errorf("cursor is on %q, want %q", got, tt.want)
			}
			if sel := d.mgr.GetSession(d.m.selectedID); sel == nil || sel.Name != tt.want {
				t.Errorf("selectedID = %q, want the session %q", d.m.selectedID, tt.want)
			}
			wantID := d.idOf(tt.want)
			calls := d.pane.waitCalls(t, 2)
			if want := []string{"focus " + string(wantID), "right"}; len(calls) < 2 || calls[len(calls)-2] != want[0] || calls[len(calls)-1] != want[1] {
				t.Errorf("right pane calls = %q, want them to end with %q", calls, want)
			}
			d.assertCursorDrawn()
		})
	}
}

// A failed operation leaves the filter, the selection and the right pane alone.
func TestUpdate_FailedStartChangesNothing(t *testing.T) {
	for _, msg := range []tea.Msg{
		sessionResumedMsg{sessionID: "x", err: errors.New("boom")},
		sessionCreatedMsg{err: errors.New("boom")},
		sessionForkedMsg{err: errors.New("boom")},
	} {
		t.Run(fmt.Sprintf("%T", msg), func(t *testing.T) {
			d := newDashboard(t, 3, 30)
			d.key("/")
			d.key("name-01")
			d.key("enter")
			before := d.m.selectedID
			callsBefore, _ := d.pane.snapshot()

			d.send(msg)

			if d.m.filterText != "name-01" || d.m.selectedID != before {
				t.Errorf("filter=%q selected=%q, want name-01 and %q", d.m.filterText, d.m.selectedID, before)
			}
			if !strings.Contains(d.footer(), "boom") {
				t.Errorf("footer %q lacks the error", d.footer())
			}
			if calls, _ := d.pane.snapshot(); len(calls) != len(callsBefore) {
				t.Errorf("right pane calls = %q, want none after the failure", calls[len(callsBefore):])
			}
		})
	}
}

func (d *dashboard) addSession(name string) session.DeckSessionID {
	s := newNamedSession(name)
	d.mgr.sessions = append(d.mgr.sessions, s)
	return s.ID
}

func (d *dashboard) idOf(name string) session.DeckSessionID {
	for _, s := range d.mgr.sessions {
		if s.Name == name {
			return s.ID
		}
	}
	d.t.Fatalf("no session %q", name)
	return ""
}

const quitPrompt = "終了しますか? (y/n)"

func TestUpdate_QuitPrompt(t *testing.T) {
	tests := []struct {
		name       string
		steps      func(d *dashboard)
		wantPrompt bool
		wantQuit   bool
		wantCursor int // -1 = do not check
	}{
		{
			name: "a clear tick of an earlier message does not hide the prompt",
			steps: func(d *dashboard) {
				d.key("?")
				gen := d.m.statusGen
				d.key("ctrl+c")
				d.send(statusClearMsg{gen: gen})
			},
			wantPrompt: true, wantCursor: -1,
		},
		{
			name: "a result arriving does not hide the prompt",
			steps: func(d *dashboard) {
				d.key("ctrl+c")
				d.send(sessionKilledMsg{})
			},
			wantPrompt: true, wantCursor: -1,
		},
		{
			name:     "y quits",
			steps:    func(d *dashboard) { d.key("ctrl+c"); d.key("y") },
			wantQuit: true, wantCursor: -1,
		},
		{
			name:       "another key cancels and is dropped",
			steps:      func(d *dashboard) { d.key("gg"); d.key("ctrl+c"); d.key("j") },
			wantCursor: 0,
		},
		{
			name: "after a stale clear, the next key cancels and the one after works",
			steps: func(d *dashboard) {
				d.key("gg")
				d.key("?")
				gen := d.m.statusGen
				d.key("ctrl+c")
				d.send(statusClearMsg{gen: gen})
				d.key("j")
				d.key("j")
			},
			wantCursor: 1,
		},
		{
			name:     "ctrl+z quits without asking",
			steps:    func(d *dashboard) { d.key("ctrl+z") },
			wantQuit: true, wantCursor: -1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDashboard(t, 3, 30)
			tt.steps(d)

			if d.quit != tt.wantQuit {
				t.Errorf("quit = %v, want %v", d.quit, tt.wantQuit)
			}
			if tt.wantQuit {
				return
			}
			if got := strings.Contains(d.footer(), quitPrompt); got != tt.wantPrompt {
				t.Errorf("prompt shown = %v, want %v (footer %q)", got, tt.wantPrompt, d.footer())
			}
			if tt.wantCursor >= 0 && d.m.cursor != tt.wantCursor {
				t.Errorf("cursor = %d, want %d", d.m.cursor, tt.wantCursor)
			}
		})
	}
}

// A flash message goes with its own clear tick, not with an earlier one.
func TestUpdate_StatusClear(t *testing.T) {
	d := newDashboard(t, 3, 30)

	d.send(sessionKilledMsg{err: errors.New("first")})
	first := d.m.statusGen
	d.send(sessionKilledMsg{err: errors.New("second")})
	second := d.m.statusGen

	d.send(statusClearMsg{gen: first})
	if !strings.Contains(d.footer(), "second") {
		t.Fatalf("the clear of the first message removed the second: footer %q", d.footer())
	}
	d.send(statusClearMsg{gen: second})
	if strings.Contains(d.footer(), "second") {
		t.Fatalf("the second message stayed after its own clear: footer %q", d.footer())
	}
}

// r starts only what CanResume allows; x and p refuse an external session.
func TestDashboard_KeysByOriginAndStatus(t *testing.T) {
	tests := []struct {
		name   string
		origin session.Origin
		status session.Status
		key    string
		want   string // part of the footer
	}{
		{"r on a completed session", session.OriginDeck, session.StatusCompleted, "r", "再開中"},
		{"r on an external session adopts it", session.OriginExternal, session.StatusUnknown, "r", "再開中"},
		{"r on an unknown status is refused", session.OriginDeck, session.StatusUnknown, "r", "再開できません"},
		{"r on a running session is refused", session.OriginDeck, session.StatusRunning, "r", "再開できません"},
		{"Enter on a completed session resumes", session.OriginDeck, session.StatusCompleted, "enter", "再開中"},
		{"x on an external session is refused", session.OriginExternal, session.StatusUnknown, "x", "終了できません"},
		{"x on an unknown status closes", session.OriginDeck, session.StatusUnknown, "x", "終了中"},
		{"p on an external session is refused", session.OriginExternal, session.StatusUnknown, "p", "ピンできません"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDashboard(t, 1, 30)
			s := d.mgr.sessions[0]
			s.Origin, s.Status = tt.origin, tt.status
			d.send(SessionRefreshMsg{})
			d.key(tt.key)
			if !strings.Contains(d.footer(), tt.want) {
				t.Errorf("footer = %q, want it to contain %q", d.footer(), tt.want)
			}
		})
	}
}
