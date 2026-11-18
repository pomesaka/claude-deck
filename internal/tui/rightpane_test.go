package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pomesaka/claude-deck/internal/preview"
	"github.com/pomesaka/claude-deck/internal/session"
)

// fakeRightPane records the I/O the worker runs, can hold the first I/O until
// release is closed, and counts how many I/O run at once.
type fakeRightPane struct {
	mu          sync.Mutex
	calls       []string
	inFlight    int
	maxInFlight int
	failFirst   bool

	hold    chan struct{} // nil = do not hold
	held    chan struct{} // closed when the first I/O is being held
	holdOne sync.Once
}

func newFakeRightPane(hold bool) *fakeRightPane {
	f := &fakeRightPane{held: make(chan struct{})}
	if hold {
		f.hold = make(chan struct{})
	}
	return f
}

func (f *fakeRightPane) do(call string) error {
	f.mu.Lock()
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	first := len(f.calls) == 0
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	if first && f.hold != nil {
		f.holdOne.Do(func() { close(f.held) })
		<-f.hold
	}

	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
	if first && f.failFirst {
		return errors.New("tmux failed")
	}
	return nil
}

func (f *fakeRightPane) ops() rightPaneOps {
	return rightPaneOps{
		focusSession: func(id session.DeckSessionID) error { return f.do("focus " + string(id)) },
		writeSpec:    func(s preview.PreviewSpec) error { return f.do("spec " + string(s.DeckSessionID) + " " + s.Status) },
		focusPreview: func() error { return f.do("preview") },
		focusRight:   func() error { return f.do("right") },
	}
}

func (f *fakeRightPane) snapshot() (calls []string, maxInFlight int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls), f.maxInFlight
}

// waitCalls waits until n calls have been made, then a little longer so that
// a call that should not happen has the chance to show up.
func (f *fakeRightPane) waitCalls(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		calls, _ := f.snapshot()
		if len(calls) >= n {
			time.Sleep(20 * time.Millisecond)
			calls, _ = f.snapshot()
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited for %d calls, got %q", n, calls)
		}
		time.Sleep(time.Millisecond)
	}
}

type paneHarness struct {
	w      *rightPaneWorker
	f      *fakeRightPane
	gen    *atomic.Uint64
	cancel context.CancelFunc
}

func newPaneHarness(t *testing.T, hold bool) *paneHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &paneHarness{f: newFakeRightPane(hold), gen: &atomic.Uint64{}, cancel: cancel}
	h.w = newRightPaneWorker(ctx, h.gen, h.f.ops())
	t.Cleanup(func() {
		if h.f.hold != nil {
			select {
			case <-h.f.hold:
			default:
				close(h.f.hold)
			}
		}
	})
	return h
}

// switchTo builds a switch the way the Model does: each one is a new generation.
func (h *paneHarness) switchTo(id string, target rightPaneTarget, focusRight bool) {
	sw := rightPaneSwitch{
		SessionID:  session.DeckSessionID(id),
		Target:     target,
		FocusRight: focusRight,
		Generation: h.gen.Add(1),
	}
	if target == rightPanePreview {
		sw.Spec = preview.PreviewSpec{DeckSessionID: session.DeckSessionID(id), Status: "cursor"}
	}
	h.w.submit(sw)
}

// refresh rewrites the spec of the session shown now (SessionRefreshMsg).
func (h *paneHarness) refresh(id, status string) {
	h.w.submitSpec(preview.PreviewSpec{DeckSessionID: session.DeckSessionID(id), Status: status}, h.gen.Load())
}

func (h *paneHarness) release() { close(h.f.hold) }

func TestRightPaneWorker(t *testing.T) {
	tests := []struct {
		name string
		hold bool // hold the first I/O while the later requests come in
		// failFirst makes the first I/O fail.
		failFirst bool
		steps     func(h *paneHarness)
		want      []string
	}{
		{
			name:  "tmux target",
			steps: func(h *paneHarness) { h.switchTo("a", rightPaneTmux, false) },
			want:  []string{"focus a"},
		},
		{
			name:  "tmux target with focus",
			steps: func(h *paneHarness) { h.switchTo("a", rightPaneTmux, true) },
			want:  []string{"focus a", "right"},
		},
		{
			name:  "preview target",
			steps: func(h *paneHarness) { h.switchTo("a", rightPanePreview, false) },
			want:  []string{"spec a cursor", "preview"},
		},
		{
			name: "requests while busy: only the latest runs next",
			hold: true,
			steps: func(h *paneHarness) {
				h.switchTo("a", rightPaneTmux, false)
				<-h.f.held
				h.switchTo("b", rightPaneTmux, false)
				h.switchTo("c", rightPaneTmux, false)
				h.switchTo("d", rightPaneTmux, false)
				h.release()
			},
			want: []string{"focus a", "focus d"},
		},
		{
			name: "one request while busy runs after",
			hold: true,
			steps: func(h *paneHarness) {
				h.switchTo("a", rightPaneTmux, false)
				<-h.f.held
				h.switchTo("b", rightPaneTmux, false)
				h.release()
			},
			want: []string{"focus a", "focus b"},
		},
		{
			name: "a superseded switch stops before its next step",
			hold: true,
			steps: func(h *paneHarness) {
				h.switchTo("a", rightPaneTmux, true)
				<-h.f.held
				h.switchTo("b", rightPaneTmux, false)
				h.release()
			},
			want: []string{"focus a", "focus b"},
		},
		{
			name:      "an I/O error does not stop the worker",
			hold:      true,
			failFirst: true,
			steps: func(h *paneHarness) {
				h.switchTo("a", rightPaneTmux, false)
				<-h.f.held
				h.switchTo("b", rightPaneTmux, false)
				h.release()
			},
			want: []string{"focus a", "focus b"},
		},
		{
			name: "refresh does not replace a pending switch",
			hold: true,
			steps: func(h *paneHarness) {
				h.switchTo("a", rightPaneTmux, false)
				<-h.f.held
				h.switchTo("b", rightPaneTmux, true)
				h.refresh("b", "refreshed")
				h.release()
			},
			want: []string{"focus a", "focus b", "right"},
		},
		{
			name: "refresh of the pending preview session updates its spec",
			hold: true,
			steps: func(h *paneHarness) {
				h.switchTo("a", rightPaneTmux, false)
				<-h.f.held
				h.switchTo("b", rightPanePreview, false)
				h.refresh("b", "refreshed")
				h.release()
			},
			want: []string{"focus a", "spec b refreshed", "preview"},
		},
		{
			name: "a cursor move after a refresh wins",
			hold: true,
			steps: func(h *paneHarness) {
				h.switchTo("a", rightPanePreview, false)
				<-h.f.held
				h.refresh("a", "refreshed")
				h.switchTo("b", rightPaneTmux, false)
				h.release()
			},
			want: []string{"spec a cursor", "focus b"},
		},
		{
			name: "refresh alone rewrites the spec without selecting a window",
			steps: func(h *paneHarness) {
				h.refresh("a", "refreshed")
			},
			want: []string{"spec a refreshed"},
		},
		{
			name: "a pending switch is dropped when the generation moves on (repo selector)",
			hold: true,
			steps: func(h *paneHarness) {
				h.switchTo("a", rightPaneTmux, false)
				<-h.f.held
				h.switchTo("b", rightPaneTmux, false)
				h.gen.Add(1)
				h.release()
			},
			want: []string{"focus a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newPaneHarness(t, tt.hold)
			h.f.failFirst = tt.failFirst
			tt.steps(h)

			got := h.f.waitCalls(t, len(tt.want))
			if !slices.Equal(got, tt.want) {
				t.Errorf("calls = %q, want %q", got, tt.want)
			}
			if _, maxInFlight := h.f.snapshot(); maxInFlight > 1 {
				t.Errorf("%d I/O ran at once, want 1", maxInFlight)
			}
		})
	}
}

// submit must not wait for the I/O: it is called from Update.
func TestRightPaneWorker_SubmitDoesNotBlock(t *testing.T) {
	h := newPaneHarness(t, true)
	h.switchTo("a", rightPaneTmux, false)
	<-h.f.held

	done := make(chan struct{})
	go func() {
		for i := range 10 {
			h.switchTo(fmt.Sprint("s", i), rightPaneTmux, false)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("submit blocked while the worker was busy")
	}
}

func TestRightPaneWorker_StopsWithContext(t *testing.T) {
	h := newPaneHarness(t, true)
	h.switchTo("a", rightPaneTmux, false)
	<-h.f.held
	h.switchTo("b", rightPaneTmux, false)
	h.cancel()
	h.release()
	time.Sleep(20 * time.Millisecond)
	h.switchTo("c", rightPaneTmux, false)

	if got := h.f.waitCalls(t, 1); !slices.Equal(got, []string{"focus a"}) {
		t.Errorf("calls = %q, want only the one in progress at cancel", got)
	}
}
