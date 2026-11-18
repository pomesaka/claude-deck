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
