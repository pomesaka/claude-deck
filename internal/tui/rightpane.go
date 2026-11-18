package tui

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/preview"
	"github.com/pomesaka/claude-deck/internal/session"
)

// rightPaneOps are the external side effects of a right pane switch.
// Tests replace them to record the order and to slow them down.
type rightPaneOps struct {
	focusSession func(session.DeckSessionID) error // tmux select-window of the session
	writeSpec    func(preview.PreviewSpec) error   // tell the preview process what to show
	focusPreview func() error                      // tmux select-window of the preview
	focusRight   func() error                      // move the Ghostty focus to the right pane
}

// rightPaneJob is one request to the worker. specOnly rewrites the preview spec
// of the session already on the right pane without selecting any window.
type rightPaneJob struct {
	sw       rightPaneSwitch
	specOnly bool
}

// rightPaneWorker runs right pane switches one at a time.
//
// WHY 1 つの goroutine: tmux の select-window や spec の書き込みは「最後の選択が勝つ」
// べき副作用で、カーソル移動ごとに tea.Cmd を並列に出すと完了順が入れ替わり、カーソルと
// 別のセッションが右ペインに残る。世代番号を確かめるだけでは、始まった I/O の後勝ちを
// 止められない（docs/concurrency.md）。worker は保留を 1 つだけ持ち、新しい要求で上書きする。
type rightPaneWorker struct {
	ops    rightPaneOps
	latest *atomic.Uint64 // Model.rightPaneGeneration

	mu      sync.Mutex
	pending *rightPaneJob
	wake    chan struct{}
}

func newRightPaneWorker(ctx context.Context, latest *atomic.Uint64, ops rightPaneOps) *rightPaneWorker {
	w := &rightPaneWorker{ops: ops, latest: latest, wake: make(chan struct{}, 1)}
	go w.run(ctx)
	return w
}

// submit queues a switch, replacing any switch that has not started yet.
func (w *rightPaneWorker) submit(sw rightPaneSwitch) {
	w.enqueue(rightPaneJob{sw: sw})
}

// submitSpec queues a rewrite of the preview spec for the session shown now.
// It never replaces a pending switch: that switch writes a spec of its own, and
// dropping it would lose the window selection. When the pending switch shows
// the same session in the preview, the newer spec is folded into it.
func (w *rightPaneWorker) submitSpec(spec preview.PreviewSpec, generation uint64) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if p := w.pending; p != nil {
		if !p.specOnly && p.sw.Target == rightPanePreview && p.sw.SessionID == spec.DeckSessionID {
			p.sw.Spec = spec
		}
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	w.enqueue(rightPaneJob{
		sw:       rightPaneSwitch{SessionID: spec.DeckSessionID, Target: rightPanePreview, Spec: spec, Generation: generation},
		specOnly: true,
	})
}

func (w *rightPaneWorker) enqueue(job rightPaneJob) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.pending = &job
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *rightPaneWorker) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		}
		for ctx.Err() == nil {
			w.mu.Lock()
			job := w.pending
			w.pending = nil
			w.mu.Unlock()
			if job == nil {
				break
			}
			w.exec(*job)
		}
	}
}

// stale reports whether a newer switch has been requested since sw was built.
// It is checked before every step, so a superseded switch stops as soon as the
// step in progress ends.
func (w *rightPaneWorker) stale(sw rightPaneSwitch) bool {
	return w.latest != nil && w.latest.Load() != sw.Generation
}

func (w *rightPaneWorker) exec(job rightPaneJob) {
	sw := job.sw
	if w.stale(sw) {
		debuglog.Printf("[rightPane] drop stale session=%s gen=%d", sw.SessionID, sw.Generation)
		return
	}
	debuglog.Printf("[rightPane] run session=%s target=%d specOnly=%v focusRight=%v gen=%d", sw.SessionID, sw.Target, job.specOnly, sw.FocusRight, sw.Generation)
	if sw.Target == rightPaneTmux {
		if err := w.ops.focusSession(sw.SessionID); err != nil {
			debuglog.Printf("[rightPane] tmux focus session=%s: %v", sw.SessionID, err)
		}
	} else {
		if err := w.ops.writeSpec(sw.Spec); err != nil {
			debuglog.Printf("[rightPane] preview IPC: %v", err)
		}
		if job.specOnly || w.stale(sw) {
			return
		}
		if err := w.ops.focusPreview(); err != nil {
			debuglog.Printf("[rightPane] tmux focus preview: %v", err)
		}
	}
	if !sw.FocusRight || w.stale(sw) {
		return
	}
	if err := w.ops.focusRight(); err != nil {
		debuglog.Printf("[rightPane] ghostty focus right: %v", err)
	}
}
